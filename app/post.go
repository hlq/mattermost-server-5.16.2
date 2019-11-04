// Copyright (c) 2016-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mattermost/mattermost-server/mlog"
	"github.com/mattermost/mattermost-server/model"
	"github.com/mattermost/mattermost-server/plugin"
	"github.com/mattermost/mattermost-server/store"
	"github.com/mattermost/mattermost-server/utils"
)

const (
	PENDING_POST_IDS_CACHE_SIZE = 25000
	PENDING_POST_IDS_CACHE_TTL  = 30 * time.Second
	PAGE_DEFAULT                = 0
)

func (a *App) CreatePostAsUser(post *model.Post, currentSessionId string) (*model.Post, *model.AppError) {
	// Check that channel has not been deleted
	channel, errCh := a.Srv.Store.Channel().Get(post.ChannelId, true)
	if errCh != nil {
		err := model.NewAppError("CreatePostAsUser", "api.context.invalid_param.app_error", map[string]interface{}{"Name": "post.channel_id"}, errCh.Error(), http.StatusBadRequest)
		return nil, err
	}

	if strings.HasPrefix(post.Type, model.POST_SYSTEM_MESSAGE_PREFIX) {
		err := model.NewAppError("CreatePostAsUser", "api.context.invalid_param.app_error", map[string]interface{}{"Name": "post.type"}, "", http.StatusBadRequest)
		return nil, err
	}

	if channel.DeleteAt != 0 {
		err := model.NewAppError("createPost", "api.post.create_post.can_not_post_to_deleted.error", nil, "", http.StatusBadRequest)
		return nil, err
	}

	rp, err := a.CreatePost(post, channel, true)
	if err != nil {
		if err.Id == "api.post.create_post.root_id.app_error" ||
			err.Id == "api.post.create_post.channel_root_id.app_error" ||
			err.Id == "api.post.create_post.parent_id.app_error" {
			err.StatusCode = http.StatusBadRequest
		}

		if err.Id == "api.post.create_post.town_square_read_only" {
			user, userErr := a.Srv.Store.User().Get(post.UserId)
			if userErr != nil {
				return nil, userErr
			}

			T := utils.GetUserTranslations(user.Locale)
			a.SendEphemeralPost(
				post.UserId,
				&model.Post{
					ChannelId: channel.Id,
					ParentId:  post.ParentId,
					RootId:    post.RootId,
					UserId:    post.UserId,
					Message:   T("api.post.create_post.town_square_read_only"),
					CreateAt:  model.GetMillis() + 1,
				},
			)
		}
		return nil, err
	}

	// Update the LastViewAt only if the post does not have from_webhook prop set (eg. Zapier app)
	if _, ok := post.Props["from_webhook"]; !ok {
		if _, err := a.MarkChannelsAsViewed([]string{post.ChannelId}, post.UserId, currentSessionId); err != nil {
			mlog.Error(
				"Encountered error updating last viewed",
				mlog.String("channel_id", post.ChannelId),
				mlog.String("user_id", post.UserId),
				mlog.Err(err),
			)
		}
	}

	return rp, nil
}

func (a *App) CreatePostMissingChannel(post *model.Post, triggerWebhooks bool) (*model.Post, *model.AppError) {
	channel, err := a.Srv.Store.Channel().Get(post.ChannelId, true)
	if err != nil {
		return nil, err
	}

	return a.CreatePost(post, channel, triggerWebhooks)
}

// deduplicateCreatePost attempts to make posting idempotent within a caching window.
func (a *App) deduplicateCreatePost(post *model.Post) (foundPost *model.Post, err *model.AppError) {
	// We rely on the client sending the pending post id across "duplicate" requests. If there
	// isn't one, we can't deduplicate, so allow creation normally.
	if post.PendingPostId == "" {
		return nil, nil
	}

	const unknownPostId = ""

	// Query the cache atomically for the given pending post id, saving a record if
	// it hasn't previously been seen.
	value, loaded := a.Srv.seenPendingPostIdsCache.GetOrAdd(post.PendingPostId, unknownPostId, PENDING_POST_IDS_CACHE_TTL)

	// If we were the first thread to save this pending post id into the cache,
	// proceed with create post normally.
	if !loaded {
		return nil, nil
	}

	postId := value.(string)

	// If another thread saved the cache record, but hasn't yet updated it with the actual post
	// id (because it's still saving), notify the client with an error. Ideally, we'd wait
	// for the other thread, but coordinating that adds complexity to the happy path.
	if postId == unknownPostId {
		return nil, model.NewAppError("deduplicateCreatePost", "api.post.deduplicate_create_post.pending", nil, "", http.StatusInternalServerError)
	}

	// If the other thread finished creating the post, return the created post back to the
	// client, making the API call feel idempotent.
	actualPost, err := a.GetSinglePost(postId)
	if err != nil {
		return nil, model.NewAppError("deduplicateCreatePost", "api.post.deduplicate_create_post.failed_to_get", nil, err.Error(), http.StatusInternalServerError)
	}

	mlog.Debug("Deduplicated create post", mlog.String("post_id", actualPost.Id), mlog.String("pending_post_id", post.PendingPostId))

	return actualPost, nil
}

func (a *App) CreatePost(post *model.Post, channel *model.Channel, triggerWebhooks bool) (savedPost *model.Post, err *model.AppError) {
	foundPost, err := a.deduplicateCreatePost(post)
	if err != nil {
		return nil, err
	}
	if foundPost != nil {
		return foundPost, nil
	}

	// If we get this far, we've recorded the client-provided pending post id to the cache.
	// Remove it if we fail below, allowing a proper retry by the client.
	defer func() {
		if post.PendingPostId == "" {
			return
		}

		if err != nil {
			a.Srv.seenPendingPostIdsCache.Remove(post.PendingPostId)
			return
		}

		a.Srv.seenPendingPostIdsCache.AddWithExpiresInSecs(post.PendingPostId, savedPost.Id, int64(PENDING_POST_IDS_CACHE_TTL.Seconds()))
	}()

	post.SanitizeProps()

	var pchan chan store.StoreResult
	if len(post.RootId) > 0 {
		pchan = make(chan store.StoreResult, 1)
		go func() {
			r, pErr := a.Srv.Store.Post().Get(post.RootId)
			pchan <- store.StoreResult{Data: r, Err: pErr}
			close(pchan)
		}()
	}

	user, err := a.Srv.Store.User().Get(post.UserId)
	if err != nil {
		return nil, err
	}

	if user.IsBot {
		post.AddProp("from_bot", "true")
	}

	if a.License() != nil && *a.Config().TeamSettings.ExperimentalTownSquareIsReadOnly &&
		!post.IsSystemMessage() &&
		channel.Name == model.DEFAULT_CHANNEL &&
		!a.RolesGrantPermission(user.GetRoles(), model.PERMISSION_MANAGE_SYSTEM.Id) {
		return nil, model.NewAppError("createPost", "api.post.create_post.town_square_read_only", nil, "", http.StatusForbidden)
	}

	// Verify the parent/child relationships are correct
	var parentPostList *model.PostList
	if pchan != nil {
		result := <-pchan
		if result.Err != nil {
			return nil, model.NewAppError("createPost", "api.post.create_post.root_id.app_error", nil, "", http.StatusBadRequest)
		}
		parentPostList = result.Data.(*model.PostList)
		if len(parentPostList.Posts) == 0 || !parentPostList.IsChannelId(post.ChannelId) {
			return nil, model.NewAppError("createPost", "api.post.create_post.channel_root_id.app_error", nil, "", http.StatusInternalServerError)
		}

		rootPost := parentPostList.Posts[post.RootId]
		if len(rootPost.RootId) > 0 {
			return nil, model.NewAppError("createPost", "api.post.create_post.root_id.app_error", nil, "", http.StatusBadRequest)
		}

		if post.ParentId == "" {
			post.ParentId = post.RootId
		}

		if post.RootId != post.ParentId {
			parent := parentPostList.Posts[post.ParentId]
			if parent == nil {
				return nil, model.NewAppError("createPost", "api.post.create_post.parent_id.app_error", nil, "", http.StatusInternalServerError)
			}
		}
	}

	post.Hashtags, _ = model.ParseHashtags(post.Message)

	if err = a.FillInPostProps(post, channel); err != nil {
		return nil, err
	}

	// Temporary fix so old plugins don't clobber new fields in SlackAttachment struct, see MM-13088
	if attachments, ok := post.Props["attachments"].([]*model.SlackAttachment); ok {
		jsonAttachments, err := json.Marshal(attachments)
		if err == nil {
			attachmentsInterface := []interface{}{}
			err = json.Unmarshal(jsonAttachments, &attachmentsInterface)
			post.Props["attachments"] = attachmentsInterface
		}
		if err != nil {
			mlog.Error("Could not convert post attachments to map interface.", mlog.Err(err))
		}
	}

	if pluginsEnvironment := a.GetPluginsEnvironment(); pluginsEnvironment != nil {
		var rejectionError *model.AppError
		pluginContext := a.PluginContext()
		pluginsEnvironment.RunMultiPluginHook(func(hooks plugin.Hooks) bool {
			replacementPost, rejectionReason := hooks.MessageWillBePosted(pluginContext, post)
			if rejectionReason != "" {
				id := "Post rejected by plugin. " + rejectionReason
				if rejectionReason == plugin.DismissPostError {
					id = plugin.DismissPostError
				}
				rejectionError = model.NewAppError("createPost", id, nil, "", http.StatusBadRequest)
				return false
			}
			if replacementPost != nil {
				post = replacementPost
			}

			return true
		}, plugin.MessageWillBePostedId)

		if rejectionError != nil {
			return nil, rejectionError
		}
	}

	rpost, err := a.Srv.Store.Post().Save(post)
	if err != nil {
		return nil, err
	}

	// Update the mapping from pending post id to the actual post id, for any clients that
	// might be duplicating requests.
	a.Srv.seenPendingPostIdsCache.AddWithExpiresInSecs(post.PendingPostId, rpost.Id, int64(PENDING_POST_IDS_CACHE_TTL.Seconds()))

	if pluginsEnvironment := a.GetPluginsEnvironment(); pluginsEnvironment != nil {
		a.Srv.Go(func() {
			pluginContext := a.PluginContext()
			pluginsEnvironment.RunMultiPluginHook(func(hooks plugin.Hooks) bool {
				hooks.MessageHasBeenPosted(pluginContext, rpost)
				return true
			}, plugin.MessageHasBeenPostedId)
		})
	}

	if a.IsESIndexingEnabled() {
		a.Srv.Go(func() {
			if err = a.Elasticsearch.IndexPost(rpost, channel.TeamId); err != nil {
				mlog.Error("Encountered error indexing post", mlog.String("post_id", post.Id), mlog.Err(err))
			}
		})
	}

	if a.Metrics != nil {
		a.Metrics.IncrementPostCreate()
	}

	if len(post.FileIds) > 0 {
		if err = a.attachFilesToPost(post); err != nil {
			mlog.Error("Encountered error attaching files to post", mlog.String("post_id", post.Id), mlog.Any("file_ids", post.FileIds), mlog.Err(err))
		}

		if a.Metrics != nil {
			a.Metrics.IncrementPostFileAttachment(len(post.FileIds))
		}
	}

	// Normally, we would let the API layer call PreparePostForClient, but we do it here since it also needs
	// to be done when we send the post over the websocket in handlePostEvents
	rpost = a.PreparePostForClient(rpost, true, false)

	if err := a.handlePostEvents(rpost, user, channel, triggerWebhooks, parentPostList); err != nil {
		mlog.Error("Failed to handle post events", mlog.Err(err))
	}

	return rpost, nil
}

func (a *App) attachFilesToPost(post *model.Post) *model.AppError {
	var attachedIds []string
	for _, fileId := range post.FileIds {
		err := a.Srv.Store.FileInfo().AttachToPost(fileId, post.Id, post.UserId)
		if err != nil {
			mlog.Warn("Failed to attach file to post", mlog.String("file_id", fileId), mlog.String("post_id", post.Id), mlog.Err(err))
			continue
		}

		attachedIds = append(attachedIds, fileId)
	}

	if len(post.FileIds) != len(attachedIds) {
		// We couldn't attach all files to the post, so ensure that post.FileIds reflects what was actually attached
		post.FileIds = attachedIds

		if _, err := a.Srv.Store.Post().Overwrite(post); err != nil {
			return err
		}
	}

	return nil
}

// FillInPostProps should be invoked before saving posts to fill in properties such as
// channel_mentions.
//
// If channel is nil, FillInPostProps will look up the channel corresponding to the post.
func (a *App) FillInPostProps(post *model.Post, channel *model.Channel) *model.AppError {
	channelMentions := post.ChannelMentions()
	channelMentionsProp := make(map[string]interface{})

	if len(channelMentions) > 0 {
		if channel == nil {
			postChannel, err := a.Srv.Store.Channel().GetForPost(post.Id)
			if err != nil {
				return model.NewAppError("FillInPostProps", "api.context.invalid_param.app_error", map[string]interface{}{"Name": "post.channel_id"}, err.Error(), http.StatusBadRequest)
			}
			channel = postChannel
		}

		mentionedChannels, err := a.GetChannelsByNames(channelMentions, channel.TeamId)
		if err != nil {
			return err
		}

		for _, mentioned := range mentionedChannels {
			if mentioned.Type == model.CHANNEL_OPEN {
				channelMentionsProp[mentioned.Name] = map[string]interface{}{
					"display_name": mentioned.DisplayName,
				}
			}
		}
	}

	if len(channelMentionsProp) > 0 {
		post.AddProp("channel_mentions", channelMentionsProp)
	} else if post.Props != nil {
		delete(post.Props, "channel_mentions")
	}

	return nil
}

func (a *App) handlePostEvents(post *model.Post, user *model.User, channel *model.Channel, triggerWebhooks bool, parentPostList *model.PostList) error {
	var team *model.Team
	if len(channel.TeamId) > 0 {
		t, err := a.Srv.Store.Team().Get(channel.TeamId)
		if err != nil {
			return err
		}
		team = t
	} else {
		// Blank team for DMs
		team = &model.Team{}
	}

	a.InvalidateCacheForChannel(channel)
	a.InvalidateCacheForChannelPosts(channel.Id)

	if _, err := a.SendNotifications(post, team, channel, user, parentPostList); err != nil {
		return err
	}

	a.Srv.Go(func() {
		_, err := a.SendAutoResponseIfNecessary(channel, user)
		if err != nil {
			mlog.Error("Failed to send auto response", mlog.String("user_id", user.Id), mlog.String("post_id", post.Id), mlog.Err(err))
		}
	})

	if triggerWebhooks {
		a.Srv.Go(func() {
			if err := a.handleWebhookEvents(post, team, channel, user); err != nil {
				mlog.Error(err.Error())
			}
		})
	}

	return nil
}

func (a *App) SendEphemeralPost(userId string, post *model.Post) *model.Post {
	post.Type = model.POST_EPHEMERAL

	// fill in fields which haven't been specified which have sensible defaults
	if post.Id == "" {
		post.Id = model.NewId()
	}
	if post.CreateAt == 0 {
		post.CreateAt = model.GetMillis()
	}
	if post.Props == nil {
		post.Props = model.StringInterface{}
	}

	post.GenerateActionIds()
	message := model.NewWebSocketEvent(model.WEBSOCKET_EVENT_EPHEMERAL_MESSAGE, "", post.ChannelId, userId, nil)
	post = a.PreparePostForClient(post, true, false)
	post = model.AddPostActionCookies(post, a.PostActionCookieSecret())
	message.Add("post", post.ToJson())
	a.Publish(message)

	return post
}

func (a *App) UpdateEphemeralPost(userId string, post *model.Post) *model.Post {
	post.Type = model.POST_EPHEMERAL

	post.UpdateAt = model.GetMillis()
	if post.Props == nil {
		post.Props = model.StringInterface{}
	}

	post.GenerateActionIds()
	message := model.NewWebSocketEvent(model.WEBSOCKET_EVENT_POST_EDITED, "", post.ChannelId, userId, nil)
	post = a.PreparePostForClient(post, true, false)
	post = model.AddPostActionCookies(post, a.PostActionCookieSecret())
	message.Add("post", post.ToJson())
	a.Publish(message)

	return post
}

func (a *App) DeleteEphemeralPost(userId, postId string) {
	post := &model.Post{
		Id:       postId,
		UserId:   userId,
		Type:     model.POST_EPHEMERAL,
		DeleteAt: model.GetMillis(),
		UpdateAt: model.GetMillis(),
	}

	message := model.NewWebSocketEvent(model.WEBSOCKET_EVENT_POST_DELETED, "", "", userId, nil)
	message.Add("post", post.ToJson())
	a.Publish(message)
}

func (a *App) UpdatePost(post *model.Post, safeUpdate bool) (*model.Post, *model.AppError) {
	post.SanitizeProps()

	postLists, err := a.Srv.Store.Post().Get(post.Id)
	if err != nil {
		return nil, err
	}
	oldPost := postLists.Posts[post.Id]

	if oldPost == nil {
		err = model.NewAppError("UpdatePost", "api.post.update_post.find.app_error", nil, "id="+post.Id, http.StatusBadRequest)
		return nil, err
	}

	if oldPost.DeleteAt != 0 {
		err = model.NewAppError("UpdatePost", "api.post.update_post.permissions_details.app_error", map[string]interface{}{"PostId": post.Id}, "", http.StatusBadRequest)
		return nil, err
	}

	if oldPost.IsSystemMessage() {
		err = model.NewAppError("UpdatePost", "api.post.update_post.system_message.app_error", nil, "id="+post.Id, http.StatusBadRequest)
		return nil, err
	}

	if a.License() != nil {
		if *a.Config().ServiceSettings.PostEditTimeLimit != -1 && model.GetMillis() > oldPost.CreateAt+int64(*a.Config().ServiceSettings.PostEditTimeLimit*1000) && post.Message != oldPost.Message {
			err = model.NewAppError("UpdatePost", "api.post.update_post.permissions_time_limit.app_error", map[string]interface{}{"timeLimit": *a.Config().ServiceSettings.PostEditTimeLimit}, "", http.StatusBadRequest)
			return nil, err
		}
	}

	channel, err := a.GetChannel(oldPost.ChannelId)
	if err != nil {
		return nil, err
	}

	if channel.DeleteAt != 0 {
		return nil, model.NewAppError("UpdatePost", "api.post.update_post.can_not_update_post_in_deleted.error", nil, "", http.StatusBadRequest)
	}

	newPost := &model.Post{}
	*newPost = *oldPost

	if newPost.Message != post.Message {
		newPost.Message = post.Message
		newPost.EditAt = model.GetMillis()
		newPost.Hashtags, _ = model.ParseHashtags(post.Message)
	}

	if !safeUpdate {
		newPost.IsPinned = post.IsPinned
		newPost.HasReactions = post.HasReactions
		newPost.FileIds = post.FileIds
		newPost.Props = post.Props
	}

	// Avoid deep-equal checks if EditAt was already modified through message change
	if newPost.EditAt == oldPost.EditAt && (!oldPost.FileIds.Equals(newPost.FileIds) || !oldPost.AttachmentsEqual(newPost)) {
		newPost.EditAt = model.GetMillis()
	}

	if err = a.FillInPostProps(post, nil); err != nil {
		return nil, err
	}

	if pluginsEnvironment := a.GetPluginsEnvironment(); pluginsEnvironment != nil {
		var rejectionReason string
		pluginContext := a.PluginContext()
		pluginsEnvironment.RunMultiPluginHook(func(hooks plugin.Hooks) bool {
			newPost, rejectionReason = hooks.MessageWillBeUpdated(pluginContext, newPost, oldPost)
			return post != nil
		}, plugin.MessageWillBeUpdatedId)
		if newPost == nil {
			return nil, model.NewAppError("UpdatePost", "Post rejected by plugin. "+rejectionReason, nil, "", http.StatusBadRequest)
		}
	}

	rpost, err := a.Srv.Store.Post().Update(newPost, oldPost)
	if err != nil {
		return nil, err
	}

	if pluginsEnvironment := a.GetPluginsEnvironment(); pluginsEnvironment != nil {
		a.Srv.Go(func() {
			pluginContext := a.PluginContext()
			pluginsEnvironment.RunMultiPluginHook(func(hooks plugin.Hooks) bool {
				hooks.MessageHasBeenUpdated(pluginContext, newPost, oldPost)
				return true
			}, plugin.MessageHasBeenUpdatedId)
		})
	}

	if a.IsESIndexingEnabled() {
		a.Srv.Go(func() {
			channel, chanErr := a.Srv.Store.Channel().GetForPost(rpost.Id)
			if chanErr != nil {
				mlog.Error("Couldn't get channel for post for Elasticsearch indexing.", mlog.String("channel_id", rpost.ChannelId), mlog.String("post_id", rpost.Id))
				return
			}
			if err := a.Elasticsearch.IndexPost(rpost, channel.TeamId); err != nil {
				mlog.Error("Encountered error indexing post", mlog.String("post_id", post.Id), mlog.Err(err))
			}
		})
	}

	rpost = a.PreparePostForClient(rpost, false, true)

	message := model.NewWebSocketEvent(model.WEBSOCKET_EVENT_POST_EDITED, "", rpost.ChannelId, "", nil)
	message.Add("post", rpost.ToJson())
	a.Publish(message)

	a.InvalidateCacheForChannelPosts(rpost.ChannelId)

	return rpost, nil
}

func (a *App) PatchPost(postId string, patch *model.PostPatch) (*model.Post, *model.AppError) {
	post, err := a.GetSinglePost(postId)
	if err != nil {
		return nil, err
	}

	channel, err := a.GetChannel(post.ChannelId)
	if err != nil {
		return nil, err
	}

	if channel.DeleteAt != 0 {
		err = model.NewAppError("PatchPost", "api.post.patch_post.can_not_update_post_in_deleted.error", nil, "", http.StatusBadRequest)
		return nil, err
	}

	post.Patch(patch)

	updatedPost, err := a.UpdatePost(post, false)
	if err != nil {
		return nil, err
	}

	return updatedPost, nil
}

func (a *App) GetPostsPage(channelId string, page int, perPage int) (*model.PostList, *model.AppError) {
	return a.Srv.Store.Post().GetPosts(channelId, page*perPage, perPage, true)
}

func (a *App) GetPosts(channelId string, offset int, limit int) (*model.PostList, *model.AppError) {
	return a.Srv.Store.Post().GetPosts(channelId, offset, limit, true)
}

func (a *App) GetPostsEtag(channelId string) string {
	return a.Srv.Store.Post().GetEtag(channelId, true)
}

func (a *App) GetPostsSince(channelId string, time int64) (*model.PostList, *model.AppError) {
	return a.Srv.Store.Post().GetPostsSince(channelId, time, true)
}

func (a *App) GetSinglePost(postId string) (*model.Post, *model.AppError) {
	return a.Srv.Store.Post().GetSingle(postId)
}

func (a *App) GetPostThread(postId string) (*model.PostList, *model.AppError) {
	return a.Srv.Store.Post().Get(postId)
}

func (a *App) GetFlaggedPosts(userId string, offset int, limit int) (*model.PostList, *model.AppError) {
	return a.Srv.Store.Post().GetFlaggedPosts(userId, offset, limit)
}

func (a *App) GetFlaggedPostsForTeam(userId, teamId string, offset int, limit int) (*model.PostList, *model.AppError) {
	return a.Srv.Store.Post().GetFlaggedPostsForTeam(userId, teamId, offset, limit)
}

func (a *App) GetFlaggedPostsForChannel(userId, channelId string, offset int, limit int) (*model.PostList, *model.AppError) {
	return a.Srv.Store.Post().GetFlaggedPostsForChannel(userId, channelId, offset, limit)
}

func (a *App) GetPermalinkPost(postId string, userId string) (*model.PostList, *model.AppError) {
	list, err := a.Srv.Store.Post().Get(postId)
	if err != nil {
		return nil, err
	}

	if len(list.Order) != 1 {
		return nil, model.NewAppError("getPermalinkTmp", "api.post_get_post_by_id.get.app_error", nil, "", http.StatusNotFound)
	}
	post := list.Posts[list.Order[0]]

	channel, err := a.GetChannel(post.ChannelId)
	if err != nil {
		return nil, err
	}

	if err = a.JoinChannel(channel, userId); err != nil {
		return nil, err
	}

	return list, nil
}

func (a *App) GetPostsBeforePost(channelId, postId string, page, perPage int) (*model.PostList, *model.AppError) {
	return a.Srv.Store.Post().GetPostsBefore(channelId, postId, perPage, page*perPage)
}

func (a *App) GetPostsAfterPost(channelId, postId string, page, perPage int) (*model.PostList, *model.AppError) {
	return a.Srv.Store.Post().GetPostsAfter(channelId, postId, perPage, page*perPage)
}

func (a *App) GetPostsAroundPost(postId, channelId string, offset, limit int, before bool) (*model.PostList, *model.AppError) {
	if before {
		return a.Srv.Store.Post().GetPostsBefore(channelId, postId, limit, offset)
	}
	return a.Srv.Store.Post().GetPostsAfter(channelId, postId, limit, offset)
}

func (a *App) GetPostAfterTime(channelId string, time int64) (*model.Post, *model.AppError) {
	return a.Srv.Store.Post().GetPostAfterTime(channelId, time)
}

func (a *App) GetPostIdAfterTime(channelId string, time int64) (string, *model.AppError) {
	return a.Srv.Store.Post().GetPostIdAfterTime(channelId, time)
}

func (a *App) GetPostIdBeforeTime(channelId string, time int64) (string, *model.AppError) {
	return a.Srv.Store.Post().GetPostIdBeforeTime(channelId, time)
}

func (a *App) GetNextPostIdFromPostList(postList *model.PostList) string {
	if len(postList.Order) > 0 {
		firstPostId := postList.Order[0]
		firstPost := postList.Posts[firstPostId]
		nextPostId, err := a.GetPostIdAfterTime(firstPost.ChannelId, firstPost.CreateAt)
		if err != nil {
			mlog.Warn("GetNextPostIdFromPostList: failed in getting next post", mlog.Err(err))
		}

		return nextPostId
	}

	return ""
}

func (a *App) GetPrevPostIdFromPostList(postList *model.PostList) string {
	if len(postList.Order) > 0 {
		lastPostId := postList.Order[len(postList.Order)-1]
		lastPost := postList.Posts[lastPostId]
		previousPostId, err := a.GetPostIdBeforeTime(lastPost.ChannelId, lastPost.CreateAt)
		if err != nil {
			mlog.Warn("GetPrevPostIdFromPostList: failed in getting previous post", mlog.Err(err))
		}

		return previousPostId
	}

	return ""
}

// AddCursorIdsForPostList adds NextPostId and PrevPostId as cursor to the PostList.
// The conditional blocks ensure that it sets those cursor IDs immediately as afterPost, beforePost or empty,
// and only query to database whenever necessary.
func (a *App) AddCursorIdsForPostList(originalList *model.PostList, afterPost, beforePost string, since int64, page, perPage int) {
	prevPostIdSet := false
	prevPostId := ""
	nextPostIdSet := false
	nextPostId := ""

	if since > 0 { // "since" query to return empty NextPostId and PrevPostId
		nextPostIdSet = true
		prevPostIdSet = true
	} else if afterPost != "" {
		if page == 0 {
			prevPostId = afterPost
			prevPostIdSet = true
		}

		if len(originalList.Order) < perPage {
			nextPostIdSet = true
		}
	} else if beforePost != "" {
		if page == 0 {
			nextPostId = beforePost
			nextPostIdSet = true
		}

		if len(originalList.Order) < perPage {
			prevPostIdSet = true
		}
	}

	if !nextPostIdSet {
		nextPostId = a.GetNextPostIdFromPostList(originalList)
	}

	if !prevPostIdSet {
		prevPostId = a.GetPrevPostIdFromPostList(originalList)
	}

	originalList.NextPostId = nextPostId
	originalList.PrevPostId = prevPostId
}

func (a *App) GetPostsForChannelAroundLastUnread(channelId, userId string, limitBefore, limitAfter int) (*model.PostList, *model.AppError) {
	var member *model.ChannelMember
	var err *model.AppError
	if member, err = a.GetChannelMember(channelId, userId); err != nil {
		return nil, err
	} else if member.LastViewedAt == 0 {
		return model.NewPostList(), nil
	}

	lastUnreadPostId, err := a.GetPostIdAfterTime(channelId, member.LastViewedAt)
	if err != nil {
		return nil, err
	} else if lastUnreadPostId == "" {
		return model.NewPostList(), nil
	}

	postList, err := a.GetPostThread(lastUnreadPostId)
	if err != nil {
		return nil, err
	}
	// Reset order to only include the last unread post: if the thread appears in the centre
	// channel organically, those replies will be added below.
	postList.Order = []string{lastUnreadPostId}

	if postListBefore, err := a.GetPostsBeforePost(channelId, lastUnreadPostId, PAGE_DEFAULT, limitBefore); err != nil {
		return nil, err
	} else if postListBefore != nil {
		postList.Extend(postListBefore)
	}

	if postListAfter, err := a.GetPostsAfterPost(channelId, lastUnreadPostId, PAGE_DEFAULT, limitAfter-1); err != nil {
		return nil, err
	} else if postListAfter != nil {
		postList.Extend(postListAfter)
	}

	postList.SortByCreateAt()
	return postList, nil
}

func (a *App) DeletePost(postId, deleteByID string) (*model.Post, *model.AppError) {
	post, err := a.Srv.Store.Post().GetSingle(postId)
	if err != nil {
		err.StatusCode = http.StatusBadRequest
		return nil, err
	}

	channel, err := a.GetChannel(post.ChannelId)
	if err != nil {
		return nil, err
	}

	if channel.DeleteAt != 0 {
		err := model.NewAppError("DeletePost", "api.post.delete_post.can_not_delete_post_in_deleted.error", nil, "", http.StatusBadRequest)
		return nil, err
	}

	if err := a.Srv.Store.Post().Delete(postId, model.GetMillis(), deleteByID); err != nil {
		return nil, err
	}

	message := model.NewWebSocketEvent(model.WEBSOCKET_EVENT_POST_DELETED, "", post.ChannelId, "", nil)
	message.Add("post", a.PreparePostForClient(post, false, false).ToJson())
	a.Publish(message)

	a.Srv.Go(func() {
		a.DeletePostFiles(post)
	})
	a.Srv.Go(func() {
		a.DeleteFlaggedPosts(post.Id)
	})

	if a.IsESIndexingEnabled() {
		a.Srv.Go(func() {
			if err := a.Elasticsearch.DeletePost(post); err != nil {
				mlog.Error("Encountered error deleting post", mlog.String("post_id", post.Id), mlog.Err(err))
			}
		})
	}

	a.InvalidateCacheForChannelPosts(post.ChannelId)

	return post, nil
}

func (a *App) DeleteFlaggedPosts(postId string) {
	if err := a.Srv.Store.Preference().DeleteCategoryAndName(model.PREFERENCE_CATEGORY_FLAGGED_POST, postId); err != nil {
		mlog.Warn("Unable to delete flagged post preference when deleting post.", mlog.Err(err))
		return
	}
}

func (a *App) DeletePostFiles(post *model.Post) {
	if len(post.FileIds) == 0 {
		return
	}

	if _, err := a.Srv.Store.FileInfo().DeleteForPost(post.Id); err != nil {
		mlog.Warn("Encountered error when deleting files for post", mlog.String("post_id", post.Id), mlog.Err(err))
	}
}

func (a *App) parseAndFetchChannelIdByNameFromInFilter(channelName, userId, teamId string, includeDeleted bool) (*model.Channel, error) {
	if strings.HasPrefix(channelName, "@") && strings.Contains(channelName, ",") {
		var userIds []string
		users, err := a.GetUsersByUsernames(strings.Split(channelName[1:], ","), false, nil)
		if err != nil {
			return nil, err
		}
		for _, user := range users {
			userIds = append(userIds, user.Id)
		}

		channel, err := a.GetGroupChannel(userIds)
		if err != nil {
			return nil, err
		}
		return channel, nil
	}

	if strings.HasPrefix(channelName, "@") && !strings.Contains(channelName, ",") {
		user, err := a.GetUserByUsername(channelName[1:])
		if err != nil {
			return nil, err
		}
		channel, err := a.GetOrCreateDirectChannel(userId, user.Id)
		if err != nil {
			return nil, err
		}
		return channel, nil
	}

	channel, err := a.GetChannelByName(channelName, teamId, includeDeleted)
	if err != nil {
		return nil, err
	}
	return channel, nil
}

func (a *App) searchPostsInTeam(teamId string, userId string, paramsList []*model.SearchParams, modifierFun func(*model.SearchParams)) (*model.PostList, *model.AppError) {
	var wg sync.WaitGroup

	pchan := make(chan store.StoreResult, len(paramsList))

	for _, params := range paramsList {
		// Don't allow users to search for everything.
		if params.Terms == "*" {
			continue
		}
		modifierFun(params)
		wg.Add(1)

		go func(params *model.SearchParams) {
			defer wg.Done()
			postList, err := a.Srv.Store.Post().Search(teamId, userId, params)
			pchan <- store.StoreResult{Data: postList, Err: err}
		}(params)
	}

	wg.Wait()
	close(pchan)

	posts := model.NewPostList()

	for result := range pchan {
		if result.Err != nil {
			return nil, result.Err
		}
		data := result.Data.(*model.PostList)
		posts.Extend(data)
	}

	posts.SortByCreateAt()
	return posts, nil
}

func (a *App) convertChannelNamesToChannelIds(channels []string, userId string, teamId string, includeDeletedChannels bool) []string {
	for idx, channelName := range channels {
		channel, err := a.parseAndFetchChannelIdByNameFromInFilter(channelName, userId, teamId, includeDeletedChannels)
		if err != nil {
			mlog.Error("error getting channel id by name from in filter", mlog.Err(err))
			continue
		}
		channels[idx] = channel.Id
	}
	return channels
}

func (a *App) convertUserNameToUserIds(usernames []string) []string {
	for idx, username := range usernames {
		if user, err := a.GetUserByUsername(username); err != nil {
			mlog.Error("error getting user by username", mlog.String("user_name", username), mlog.Err(err))
		} else {
			usernames[idx] = user.Id
		}
	}
	return usernames
}

func (a *App) SearchPostsInTeam(teamId string, paramsList []*model.SearchParams) (*model.PostList, *model.AppError) {
	if !*a.Config().ServiceSettings.EnablePostSearch {
		return nil, model.NewAppError("SearchPostsInTeam", "store.sql_post.search.disabled", nil, fmt.Sprintf("teamId=%v", teamId), http.StatusNotImplemented)
	}
	return a.searchPostsInTeam(teamId, "", paramsList, func(params *model.SearchParams) {
		params.SearchWithoutUserId = true
	})
}

func (a *App) esSearchPostsInTeamForUser(paramsList []*model.SearchParams, userId, teamId string, isOrSearch, includeDeletedChannels bool, page, perPage int) (*model.PostSearchResults, *model.AppError) {
	finalParamsList := []*model.SearchParams{}
	includeDeleted := includeDeletedChannels && *a.Config().TeamSettings.ExperimentalViewArchivedChannels

	for _, params := range paramsList {
		params.OrTerms = isOrSearch
		// Don't allow users to search for "*"
		if params.Terms != "*" {
			// Convert channel names to channel IDs
			params.InChannels = a.convertChannelNamesToChannelIds(params.InChannels, userId, teamId, includeDeletedChannels)
			params.ExcludedChannels = a.convertChannelNamesToChannelIds(params.ExcludedChannels, userId, teamId, includeDeletedChannels)

			// Convert usernames to user IDs
			params.FromUsers = a.convertUserNameToUserIds(params.FromUsers)
			params.ExcludedUsers = a.convertUserNameToUserIds(params.ExcludedUsers)

			finalParamsList = append(finalParamsList, params)
		}
	}

	// If the processed search params are empty, return empty search results.
	if len(finalParamsList) == 0 {
		return model.MakePostSearchResults(model.NewPostList(), nil), nil
	}

	// We only allow the user to search in channels they are a member of.
	userChannels, err := a.GetChannelsForUser(teamId, userId, includeDeleted)
	if err != nil {
		mlog.Error("error getting channel for user", mlog.Err(err))
		return nil, err
	}

	postIds, matches, err := a.Elasticsearch.SearchPosts(userChannels, finalParamsList, page, perPage)
	if err != nil {
		return nil, err
	}

	// Get the posts
	postList := model.NewPostList()
	if len(postIds) > 0 {
		posts, err := a.Srv.Store.Post().GetPostsByIds(postIds)
		if err != nil {
			return nil, err
		}
		for _, p := range posts {
			if p.DeleteAt == 0 {
				postList.AddPost(p)
				postList.AddOrder(p.Id)
			}
		}
	}

	return model.MakePostSearchResults(postList, matches), nil
}

func (a *App) SearchPostsInTeamForUser(terms string, userId string, teamId string, isOrSearch bool, includeDeletedChannels bool, timeZoneOffset int, page, perPage int) (*model.PostSearchResults, *model.AppError) {
	var postSearchResults *model.PostSearchResults
	var err *model.AppError
	paramsList := model.ParseSearchParams(strings.TrimSpace(terms), timeZoneOffset)

	if !*a.Config().ServiceSettings.EnablePostSearch {
		return nil, model.NewAppError("SearchPostsInTeamForUser", "store.sql_post.search.disabled", nil, fmt.Sprintf("teamId=%v userId=%v", teamId, userId), http.StatusNotImplemented)
	}

	if a.IsESSearchEnabled() {
		postSearchResults, err = a.esSearchPostsInTeamForUser(paramsList, userId, teamId, isOrSearch, includeDeletedChannels, page, perPage)
		if err != nil {
			mlog.Error("Encountered error on SearchPostsInTeamForUser through Elasticsearch. Falling back to default search.", mlog.Err(err))
		}
	}

	if !a.IsESSearchEnabled() || err != nil {
		// Since we don't support paging for DB search, we just return nothing for later pages
		if page > 0 {
			return model.MakePostSearchResults(model.NewPostList(), nil), nil
		}

		includeDeleted := includeDeletedChannels && *a.Config().TeamSettings.ExperimentalViewArchivedChannels
		posts, err := a.searchPostsInTeam(teamId, userId, paramsList, func(params *model.SearchParams) {
			params.IncludeDeletedChannels = includeDeleted
			params.OrTerms = isOrSearch
			for idx, channelName := range params.InChannels {
				if strings.HasPrefix(channelName, "@") {
					channel, err := a.parseAndFetchChannelIdByNameFromInFilter(channelName, userId, teamId, includeDeletedChannels)
					if err != nil {
						mlog.Error("error getting channel_id by name from in filter", mlog.Err(err))
						continue
					}
					params.InChannels[idx] = channel.Name
				}
			}
			for idx, channelName := range params.ExcludedChannels {
				if strings.HasPrefix(channelName, "@") {
					channel, err := a.parseAndFetchChannelIdByNameFromInFilter(channelName, userId, teamId, includeDeletedChannels)
					if err != nil {
						mlog.Error("error getting channel_id by name from in filter", mlog.Err(err))
						continue
					}
					params.ExcludedChannels[idx] = channel.Name
				}
			}
		})
		if err != nil {
			return nil, err
		}

		postSearchResults = model.MakePostSearchResults(posts, nil)
	}

	return postSearchResults, nil
}

func (a *App) GetFileInfosForPostWithMigration(postId string) ([]*model.FileInfo, *model.AppError) {

	pchan := make(chan store.StoreResult, 1)
	go func() {
		post, err := a.Srv.Store.Post().GetSingle(postId)
		pchan <- store.StoreResult{Data: post, Err: err}
		close(pchan)
	}()

	infos, err := a.GetFileInfosForPost(postId, false)
	if err != nil {
		return nil, err
	}

	if len(infos) == 0 {
		// No FileInfos were returned so check if they need to be created for this post
		result := <-pchan
		if result.Err != nil {
			return nil, result.Err
		}
		post := result.Data.(*model.Post)

		if len(post.Filenames) > 0 {
			a.Srv.Store.FileInfo().InvalidateFileInfosForPostCache(postId)
			// The post has Filenames that need to be replaced with FileInfos
			infos = a.MigrateFilenamesToFileInfos(post)
		}
	}

	return infos, nil
}

func (a *App) GetFileInfosForPost(postId string, fromMaster bool) ([]*model.FileInfo, *model.AppError) {
	return a.Srv.Store.FileInfo().GetForPost(postId, fromMaster, false, true)
}

func (a *App) PostWithProxyAddedToImageURLs(post *model.Post) *model.Post {
	if f := a.ImageProxyAdder(); f != nil {
		return post.WithRewrittenImageURLs(f)
	}
	return post
}

func (a *App) PostWithProxyRemovedFromImageURLs(post *model.Post) *model.Post {
	if f := a.ImageProxyRemover(); f != nil {
		return post.WithRewrittenImageURLs(f)
	}
	return post
}

func (a *App) PostPatchWithProxyRemovedFromImageURLs(patch *model.PostPatch) *model.PostPatch {
	if f := a.ImageProxyRemover(); f != nil {
		return patch.WithRewrittenImageURLs(f)
	}
	return patch
}

func (a *App) ImageProxyAdder() func(string) string {
	if !*a.Config().ImageProxySettings.Enable {
		return nil
	}

	return func(url string) string {
		return a.Srv.ImageProxy.GetProxiedImageURL(url)
	}
}

func (a *App) ImageProxyRemover() (f func(string) string) {
	if !*a.Config().ImageProxySettings.Enable {
		return nil
	}

	return func(url string) string {
		return a.Srv.ImageProxy.GetUnproxiedImageURL(url)
	}
}

func (a *App) MaxPostSize() int {
	maxPostSize := a.Srv.Store.Post().GetMaxPostSize()
	if maxPostSize == 0 {
		return model.POST_MESSAGE_MAX_RUNES_V1
	}

	return maxPostSize
}