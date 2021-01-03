package tight

import (
	"fmt"
	"strings"

	"github.com/slack-go/slack"
)

func joinText(first string, second string, divider string) string {
	if first == "" {
		return second
	}
	if second == "" {
		return first
	}
	return first + divider + second
}

func formatMultipartyChannelName(slackChannelID string, slackChannelName string) string {
	name := "&" + slackChannelID + "|" + slackChannelName
	name = strings.Replace(name, "mpdm-", "", -1)
	name = strings.Replace(name, "--", "-", -1)
	if len(name) >= 30 {
		return name[:29] + "…"
	}
	return name
}

func getConversationDetails(
	t *tight,
	channelID string,
	timestamp string,
) (slack.Message, error) {
	response, err := t.SlackClient.GetConversationHistory(&slack.GetConversationHistoryParameters{
		ChannelID: channelID,
		Latest:    timestamp,
		Limit:     1,
		Inclusive: true,
	})

	if err != nil {
		return slack.Message{}, err
	}

	if len(response.Messages) > 0 {
		msg := response.Messages[0]
		if msg.Timestamp == timestamp {
			return msg, nil
		}
	}

	replies, _, _, err := t.SlackClient.GetConversationReplies(
		&slack.GetConversationRepliesParameters{
			ChannelID: channelID,
			Latest:    timestamp,
			Limit:     1,
			Inclusive: true,
			Timestamp: timestamp,
		},
	)

	if err != nil {
		return slack.Message{}, err
	}

	if len(replies) > 0 && replies[0].Timestamp == timestamp {
		return replies[0], nil
	}

	return slack.Message{}, fmt.Errorf("No such message found")
}

func replacePermalinkWithText(t *tight, text string) string {
	matches := rxSlackArchiveURL.FindStringSubmatch(text)
	if len(matches) != 4 {
		return text
	}
	channel := matches[1]
	timestamp := matches[2] + "." + matches[3]
	message, err := getConversationDetails(t, channel, timestamp)
	if err != nil {
		t.logF("could not get message details from permalink %s %s %s %v", matches[0], channel, timestamp, err)
		return text
	}
	return text + "\n> " + message.Text
}

func printMessage(t *tight, message slack.Msg, sufix string) *Line {
	user := t.GetUserInfo(message.User)
	name := ""
	if user == nil {
		if message.User != "" {
			name = message.User
		} else {
			name = strings.ReplaceAll(message.Username, " ", "_")
		}
	} else {
		name = user.Name
	}
	text := message.Text
	for _, attachment := range message.Attachments {
		text = joinText(text, attachment.Pretext, "\n")
		text = joinText(text, attachment.Title, "\n")
		if attachment.Text != "" {
			text = joinText(text, attachment.Text, "\n")
		} else {
			text = joinText(text, attachment.Fallback, "\n")
		}
		text = joinText(text, attachment.ImageURL, "\n")
	}

	// TODO: handle files
	// for _, file := range message.Files {
	// text = joinText(text, t.FileHandler.Download(file), " ")
	// }

	reactionsText := ""
	for _, reaction := range message.Reactions {
		if reactionsText == "" {
			reactionsText = "\n└╴"
		}
		reactionsText = reactionsText + fmt.Sprintf("[%d] %s ", reaction.Count, reaction.Name)
	}
	text = text + reactionsText
	if name == "" && text == "" {
		// log.Warningf("Empty username and message: %+v", message)
		return &Line{}
	}
	text = replacePermalinkWithText(t, text)
	text = t.ExpandUserIds(text)
	text = t.ExpandText(text)
	if message.ReplyCount > 0 {
		// t.logF("Message with replies: %v", message)
		text = fmt.Sprintf("%s\n└╴[%d] %s", text, message.ReplyCount, message.Timestamp)
	}

	if name == "" {
		name = "unknown"
	}
	return &Line{
		timestamp: message.Timestamp,
		text:      joinText(text, sufix, " "),
		author:    name,
	}
}

func (t *tight) eventHandler(rtm *slack.RTM) {
	t.log("Started Slack event listener")
	for msg := range rtm.IncomingEvents {
		switch ev := msg.Data.(type) {
		case *slack.MessageEvent:
			// https://api.slack.com/events/message
			message := ev.Msg
			if message.SubType == "message_replied" {
				continue
			}
			if message.SubType == "message_changed" && ev.PreviousMessage != nil {
				newMsg := ev.SubMessage
				newMsg.Channel = message.Channel

				chanName, err := t.createChannelNameFrom(newMsg.Channel, message.ThreadTimestamp, message.Username)

				if err != nil {
					t.logF("Failed to get the channel name: %s", err)
					continue
				}

				t.updateMsg(chanName, ev.SubMessage.Timestamp, printMessage(t, *newMsg, "(edited)"))
				continue
			}

			chanName, err := t.createChannelNameFrom(message.Channel, message.ThreadTimestamp, message.User)
			if err != nil {
				t.logF("Failed to get the channel name: %s", err)
				continue
			}

			t.newMsg(chanName, printMessage(t, message, ""))

			if message.ThreadTimestamp == "" {
				continue
			}
			threadOpener, err := t.GetThreadOpener(message.Channel, message.ThreadTimestamp)
			if err != nil {
				t.logF("Failed to find thread opener: %s", err)
				continue
			}
			originalChanName, err := t.createChannelNameFrom(message.Channel, "", message.User)
			if err != nil {
				t.logF("Failed to get the channel name: %s", err)
				continue
			}

			t.updateMsg(originalChanName, threadOpener.Timestamp, printMessage(t, threadOpener.Msg, ""))

		case *slack.ConnectedEvent:
			t.log("Connected to Slack")
		case *slack.DisconnectedEvent:
			de := msg.Data.(*slack.DisconnectedEvent)
			t.logF("Disconnected from Slack (intentional: %v, cause: %v)", de.Intentional, de.Cause)
			if !de.Intentional {
				rtm.Disconnect()
				t.connect()
			}
			return
		case *slack.TeamJoinEvent, *slack.UserChangeEvent:
			// https://api.slack.com/events/team_join
			// https://api.slack.com/events/user_change
			// Refresh the users list
			// TODO update just the new user
			t.Users.Fetch(t.SlackClient)
		case *slack.ChannelJoinedEvent:
			// https://api.slack.com/events/channel_joined
			t.registerChannel(ev.Channel)
		case *slack.ReactionAddedEvent:
			// https://api.slack.com/events/reaction_added
			threadOpener, err := t.GetThreadOpener(ev.Item.Channel, ev.Item.Timestamp)
			if err != nil {
				t.logF("Failed to find thread opener: %s", err)
				continue
			}
			originalChanName, err := t.createChannelNameFrom(ev.Item.Channel, "", "")
			if err != nil {
				t.logF("Failed to get the channel name: %s", err)
				continue
			}

			t.updateMsg(originalChanName, threadOpener.Timestamp, printMessage(t, threadOpener.Msg, ""))
		case *slack.LatencyReport:
			t.logF("Current Slack latency: %v", ev.Value)
		case *slack.RTMError:
			t.logF("Slack RTM error: %v", ev.Error())
		case *slack.InvalidAuthEvent:
			t.log("Invalid slack credentials")
		default:
			t.log(fmt.Sprintf("SLACK event: %v: %+v", msg.Type, msg.Data))
		}
	}
}
