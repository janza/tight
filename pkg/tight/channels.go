package tight

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/slack-go/slack"
	bolt "go.etcd.io/bbolt"
)

const (
	MaxSlackAPIAttempts = 3
)

// passwordToTokenAndCookie parses the password specified by the user into a
// Slack token and optionally a cookie Auth cookies can be specified by
//appending a "|" symbol and the base64-encoded auth cookie to the Slack token.
func passwordToTokenAndCookie(p string) (string, string, error) {
	parts := strings.Split(p, "|")

	switch len(parts) {
	case 1:
		// XXX should check that the token starts with xoxp- ?
		return parts[0], "", nil
	case 2:
		if !strings.HasPrefix(parts[0], "xoxc-") {
			return "", "", errors.New("auth cookie is set, but token does not start with xoxc-")
		}
		if parts[1] == "" {
			return "", "", errors.New("auth cookie is empty")
		}
		if !strings.HasPrefix(parts[1], "d=") || !strings.HasSuffix(parts[1], ";") {
			return "", "", errors.New("auth cookie must have the format 'd=XXX;'")
		}
		return parts[0], parts[1], nil
	default:
		return "", "", fmt.Errorf("failed to parse password into token and cookie, got %d components, want 1 or 2", len(parts))
	}
}

// custom HTTP client used to set the auth cookie if requested, and only over
// TLS.
type httpClient struct {
	c      http.Client
	cookie string
}

func (hc httpClient) Do(req *http.Request) (*http.Response, error) {
	if hc.cookie != "" {
		if strings.ToLower(req.URL.Scheme) == "https" {
			req.Header.Add("Cookie", hc.cookie)
		} else {
			// log.Warning("Cookie is set but connection is not HTTPS, skipping")
		}
	}
	return hc.c.Do(req)
}

func (t *tight) connect() error {
	rtm := t.SlackClient.NewRTM()
	t.SlackRTM = rtm
	go func() {
		rtm.ManageConnection()
		t.log("Manage connection ended")
	}()
	t.log("Starting Slack client")

	// Wait until the websocket is connected, then print client info
	var info *slack.Info

	// FIXME tune the timeout to a value that makes sense
	timeout := 10 * time.Second
	start := time.Now()
	for {
		if info = rtm.GetInfo(); info != nil {
			break
		}
		if time.Now().After(start.Add(timeout)) {
			return fmt.Errorf("Connection to Slack timed out after %v", timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := t.Users.Fetch(t.SlackClient); err != nil {
		t.log(fmt.Sprintf("Cannot fetch user list %v", err))
	}

	go t.eventHandler(rtm)
	return t.joinChannels()
}

func (t *tight) joinChannels() error {
	var (
		chans      []slack.Channel
		nextCursor string
		err        error
	)
	for {
		attempt := 0
		for {
			// retry if rate-limited, no more than MaxSlackAPIAttempts times
			if attempt >= MaxSlackAPIAttempts {
				return fmt.Errorf("GetConversations: exceeded the maximum number of attempts (%d) with the Slack API", MaxSlackAPIAttempts)
			}
			params := slack.GetConversationsParameters{
				Types: []string{
					"public_channel",
					"private_channel",
					"im",
					// "mpim",
				},
				Cursor:          nextCursor,
				ExcludeArchived: "true",
			}
			chans, nextCursor, err = t.SlackClient.GetConversations(&params)
			if err != nil {
				if rlErr, ok := err.(*slack.RateLimitedError); ok {
					// we were rate-limited. Let's wait as much as Slack
					// instructs us to do
					time.Sleep(rlErr.RetryAfter)
					attempt++
					continue
				}
				return fmt.Errorf("Cannot get slack channels: %v", err)
			}
			break
		}
		for _, ch := range chans {
			t.registerChannel(ch)
		}
		if nextCursor == "" {
			break
		}
	}
	return nil
}

func (t *tight) markRead(channel string) {
	t.markChannel <- channel
	t.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("lastRead"))
		err := b.Put(
			[]byte(channel),
			[]byte(fmt.Sprintf("%d", time.Now().Unix())))
		return err
	})
}

func (t *tight) registerChannel(ch slack.Channel) {
	if ch.IsMember {
		t.conversationCache[ch.ID] = &ch
		t.registerChannelName(ch.Name)
	} else if ch.User != "" {
		t.registerUserChannel(ch)
		return
	} else {
		t.logF("Skipping: %s %s", ch.Name, ch.User)
	}
}

func (t *tight) registerUserChannel(ch slack.Channel) {
	u := t.Users.ByID(ch.User)
	if u == nil {
		t.logF("User not found %s", ch.User)
		return
	}
	t.conversationCache[ch.ID] = &ch
	t.registerChannelName(u.Name)
}

func (t *tight) GetConversationInfo(channelID string) (*slack.Channel, error) {
	if c, exists := t.conversationCache[channelID]; exists {
		return c, nil
	}

	channel, err := t.SlackClient.GetConversationInfo(channelID, false)
	if err != nil {
		return nil, err
	}
	t.registerChannel(*channel)
	return channel, err
}

func (t *tight) registerChannelName(name string) {
	for _, existingChan := range t.channels {
		if existingChan == name {
			return
		}
	}
	t.channels = append(t.channels, name)
}

func (t *tight) loadHistory(channelID string) ([]*Line, error) {
	t.log(fmt.Sprintf("Loading history for: %s", channelID))
	params := slack.GetConversationHistoryParameters{
		ChannelID: channelID,
		Limit:     100,
		Inclusive: true,
		Latest:    fmt.Sprintf("%d", time.Now().Unix()),
	}
	res, err := t.SlackClient.GetConversationHistory(&params)
	if err != nil {
		t.log(fmt.Sprintf("Failed to getHistory: %v", err))
		return nil, err
	}
	r := make([]*Line, 0)
	for i := len(res.Messages) - 1; i >= 0; i-- {
		msg := res.Messages[i]
		if msg.Msg.Channel == "" {
			msg.Msg.Channel = channelID
		}
		r = append(r, printMessage(t, msg.Msg, ""))
	}
	return r, nil
}

func (t *tight) loadThreadHistory(channelID, timestamp string) ([]*Line, error) {
	t.log(fmt.Sprintf("Loading history for: %s %s", channelID, timestamp))
	replies, _, _, err := t.SlackClient.GetConversationReplies(
		&slack.GetConversationRepliesParameters{
			ChannelID: channelID,
			Latest:    fmt.Sprintf("%d", time.Now().Unix()),
			Limit:     100,
			Inclusive: true,
			Timestamp: timestamp,
		},
	)
	if err != nil {
		t.log(fmt.Sprintf("Failed to getHistory: %v", err))
		return nil, err
	}
	r := make([]*Line, 0)
	for i := 0; i < len(replies); i++ {
		msg := replies[i]
		if msg.Msg.Channel == "" {
			msg.Msg.Channel = channelID
		}
		r = append(r, printMessage(t, msg.Msg, ""))
	}
	return r, nil
}

func (t *tight) getChannelIdFromName(name string) (string, string, error) {
	t.logF("Loading channel ID from name: %s", name)
	if !strings.HasPrefix(name, ">") {
		for id, channel := range t.conversationCache {
			if channel.Name == name {
				return id, "", nil
			}
			if channel.Name == "" && channel.IsIM {
				u := t.Users.ByID(channel.User)
				if u != nil && u.Name == name {
					t.logF("Found channel ID from name: %s", channel.ID)
					return channel.ID, "", nil
				}
			}
		}
	} else {
		splitString := strings.Split(name[1:], "-")
		channelID := strings.Join(splitString[1:len(splitString)-1], "-")
		threadTimestamp := splitString[len(splitString)-1]
		return channelID, threadTimestamp, nil
	}
	return "", "", fmt.Errorf("No such channel")
}

func (t *tight) formatThreadChannelName(threadTimestamp string, channel *slack.Channel) string {
	if strings.HasPrefix(channel.ID, "D") {
		u := t.Users.ByID(channel.User)
		return ">" + u.Name + "-" + channel.ID + "-" + threadTimestamp
	}
	return ">" + channel.Name + "-" + channel.ID + "-" + threadTimestamp
}

func (t *tight) createChannelNameFrom(channelID, threadTimestamp, userId string) (string, error) {
	channel, err := t.GetConversationInfo(channelID)

	if err != nil {
		return "", err
	}

	if threadTimestamp != "" {
		return t.formatThreadChannelName(threadTimestamp, channel), nil
	} else if channel.IsMpIM {
		channame := channel.Name
		return channame, nil
	} else if strings.HasPrefix(channelID, "D") {
		u := t.Users.ByID(channel.User)
		return u.Name, nil
	}
	return channel.Name, nil
}
