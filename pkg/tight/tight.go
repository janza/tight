package tight

import (
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jroimartin/gocui"
	"github.com/slack-go/slack"
	bolt "go.etcd.io/bbolt"
)

var (
	rxSlackUrls       = regexp.MustCompile(`<[^>]+>?`)
	rxSlackUser       = regexp.MustCompile(`<@[UW][A-Z0-9]+>`)
	rxSlackArchiveURL = regexp.MustCompile(`https?:\\/\\/[a-z0-9\\-]+\\.slack\\.com\\/archives\\/([a-zA-Z0-9]+)\\/p([0-9]{10})([0-9]{6})`)
)

type tight struct {
	g                  *gocui.Gui
	logFile            *os.File
	channels           []string
	currentChannel     string
	channelFilter      string
	view               string
	highlightedChannel int
	latestUnread       map[string]int
	history            map[string][]*Line
	SlackClient        *slack.Client
	SlackRTM           *slack.RTM
	Users              *Users
	conversationCache  map[string]*slack.Channel
	markChannel        chan string
	db                 *bolt.DB
}

type Line struct {
	timestamp string
	text      string
	author    string
}

func NewTight(token string, g *gocui.Gui) (*tight, error) {
	token, cookie, err := passwordToTokenAndCookie(token)
	if err != nil {
		return nil, err
	}
	slackClient := slack.New(
		token,
		slack.OptionDebug(false),
		slack.OptionHTTPClient(&httpClient{cookie: cookie, c: http.Client{Timeout: time.Second * 5}}),
	)
	t := &tight{
		SlackClient:        slackClient,
		g:                  g,
		channels:           make([]string, 0),
		currentChannel:     "",
		channelFilter:      "",
		view:               "input",
		highlightedChannel: 0,
		latestUnread:       map[string]int{},
		history: map[string][]*Line{
			"#product": {
				{
					timestamp: "12:00",
					text:      "this is text in the channel fmdso apfmdsa pfmdsam fopdsamf opdsmaopf mdsoamf opamfp os",
					author:    "Author",
				},
			},
		},
		Users:             NewUsers(slackClient, 0),
		conversationCache: make(map[string]*slack.Channel, 0),
		markChannel:       make(chan string),
	}

	return t, nil
}

type ByFilter tight

func (a *ByFilter) Len() int      { return len(a.channels) }
func (a *ByFilter) Swap(i, j int) { a.channels[i], a.channels[j] = a.channels[j], a.channels[i] }
func (a *ByFilter) Less(i, j int) bool {
	iChan := a.channels[i]
	jChan := a.channels[j]
	iChanMatches := strings.Contains(iChan, a.channelFilter)
	jChanMatches := strings.Contains(jChan, a.channelFilter)
	if iChanMatches && !jChanMatches {
		return true
	}

	if !iChanMatches && jChanMatches {
		return false
	}

	return a.latestUnread[iChan] > a.latestUnread[jChan]
}

func (t *tight) log(log string) {
	if t.logFile == nil {
		logFile, err := os.OpenFile("tight.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return
		}
		t.logFile = logFile
	}

	fmt.Fprint(t.logFile, log+"\n")
}

func (t *tight) logF(format string, a ...interface{}) {
	t.log(fmt.Sprintf(format, a...))
}

// GetThreadOpener returns text of the first message in a thread that provided message belongs to
func (t *tight) GetThreadOpener(channelID string, threadTimestamp string) (slack.Message, error) {
	msgs, _, _, err := t.SlackClient.GetConversationReplies(&slack.GetConversationRepliesParameters{
		ChannelID: channelID,
		Timestamp: threadTimestamp,
	})
	if err != nil || len(msgs) == 0 {
		return slack.Message{}, err
	}
	return msgs[0], nil
}

func Start() {
	g, err := gocui.NewGui(gocui.OutputNormal)
	if err != nil {
		log.Panicln(err)
	}
	defer g.Close()

	// g.InputEsc = true
	g.Cursor = true

	token := os.Getenv("TOKEN")
	if token == "" {
		log.Panicln("TOKEN environment variable not provided")
	}

	t, err := NewTight(token, g)
	if err != nil {
		log.Panicln(err)
	}

	t.db, err = bolt.Open("tight.db", 0666, nil)
	if err != nil {
		log.Panicln(err)
	}
	defer t.db.Close()

	t.db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte("lastRead"))
		if err != nil {
			return fmt.Errorf("create bucket: %s", err)
		}
		return nil
	})

	g.SetManagerFunc(t.Layout)

	if err := g.SetKeybinding("", gocui.KeyCtrlC, gocui.ModNone, quit); err != nil {
		log.Panicln(err)
	}

	if err := g.SetKeybinding("", gocui.KeyCtrlP, gocui.ModNone, t.channelPicker); err != nil {
		log.Panicln(err)
	}

	if err := g.SetKeybinding("", gocui.KeyEsc, gocui.ModNone, t.normal); err != nil {
		log.Panicln(err)
	}

	go t.connect()

	if err := g.MainLoop(); err != nil && err != gocui.ErrQuit {
		log.Panicln(err)
	}
}

func (t *tight) Layout(g *gocui.Gui) error {
	maxX, maxY := g.Size()

	vLog, err := g.SetView("log", -1, 1, maxX, maxY-1)
	if err != nil {
		if err != gocui.ErrUnknownView {
			return err
		}
		vLog.Frame = false
		vLog.Wrap = true
		vLog.Autoscroll = true
		t.renderLog()
	}

	channelsSize := 1
	if t.view == "channels" {
		channelsSize = 10
	}

	if vChannels, err := g.SetView("channels", -1, -1, maxX, channelsSize); err != nil {
		if err != gocui.ErrUnknownView {
			return err
		}
		vChannels.Editable = true
		vChannels.Editor = gocui.EditorFunc(t.channelEditor)
	}

	if vInput, err := g.SetView("input", -1, maxY-2, maxX+1, maxY); err != nil {
		if err != gocui.ErrUnknownView {
			return err
		}
		vInput.Editable = true
		vInput.Editor = gocui.EditorFunc(t.simpleEditor)
	}

	currentView := g.CurrentView()
	if currentView == nil || currentView.Name() != t.view {
		t.setCurrentViewOnTop(g)
	}

	return nil
}

func (t *tight) renderLog() {
	v, _ := t.g.View("log")
	if t.currentChannel == "" {
		return
	}
	v.Clear()
	v.SetCursor(0, 0)
	v.SetOrigin(0, 0)
	_, y := v.Size()
	h := t.history[t.currentChannel]
	offset := len(h) - y
	if offset < 0 {
		offset = 0
	}
	prevTime := time.Time{}
	prevAuthor := ""
	for _, line := range h[offset:] {
		// t.log(fmt.Sprintf("%d", i))
		intTimestamp, err := strconv.ParseInt(strings.Split(line.timestamp, ".")[0], 10, 64)
		if err != nil {
			intTimestamp = 0
		}
		cTime := time.Unix(intTimestamp, 0)
		if cTime.Sub(prevTime).Minutes() > 5 || prevAuthor != line.author {
			fmt.Fprintf(v, "\033[1;4m%s %s\033[0m\n", cTime.Format("15:04"), line.author)
			prevTime = cTime
			prevAuthor = line.author
		}
		fmt.Fprintf(v, "%s\n", line.text)
	}
}

func (t *tight) newMsg(chanName string, l *Line) {
	t.registerChannelName(chanName)
	t.history[chanName] = append(t.history[chanName], l)
	if t.currentChannel != chanName {
		t.latestUnread[chanName] += 1
	}
	t.g.Update(func(g *gocui.Gui) error {
		t.renderLog()
		return nil
	})
}

func (t *tight) updateMsg(chanName, originalTime string, l *Line) {
	t.registerChannelName(chanName)
	found := false
	for i, msg := range t.history[chanName] {
		if msg.timestamp == originalTime {
			t.history[chanName][i] = l
			found = true
			break
		}
	}
	if found {
		t.logF("Updated %s %s %v", chanName, originalTime, l)
	} else {
		t.logF("Didn't update %s %s %v", chanName, originalTime, l)
	}
	// t.history[chanName] = append(t.history[chanName], l)
	t.g.Update(func(g *gocui.Gui) error {
		t.renderLog()
		return nil
	})
}

func (t *tight) threadStateUpdate(chanName, originalTime string, l *Line) {
	t.registerChannelName(chanName)
	found := false
	for i, msg := range t.history[chanName] {
		if msg.timestamp == originalTime {
			t.history[chanName][i] = l
			found = true
			break
		}
	}
	if found {
		t.logF("Updated %s %s %v", chanName, originalTime, l)
	} else {
		t.logF("Didn't update %s %s %v", chanName, originalTime, l)
	}
	// t.history[chanName] = append(t.history[chanName], l)
	t.g.Update(func(g *gocui.Gui) error {
		t.renderLog()
		return nil
	})
}

func (t *tight) selectChannel(channel string) {
	channel = strings.TrimSpace(channel)
	t.log(fmt.Sprintf("Selecting: '%s'", channel))
	t.view = "input"

	if channel == "" {
		return
	}

	t.latestUnread[channel] = 0

	if t.currentChannel == channel {
		return
	}

	t.currentChannel = channel

	if t.history[channel] == nil || len(t.history[channel]) < 10 {
		go func() {
			var history []*Line
			var err error

			channelID, threadTimestamp, err := t.getChannelIdFromName(channel)
			if err != nil {
				t.log(fmt.Sprintf("Failed to get history for channel: %s", channel))
				return
			}

			if threadTimestamp == "" {
				history, err = t.loadHistory(channelID)
			} else {
				history, err = t.loadThreadHistory(channelID, threadTimestamp)
			}
			if err != nil {
				t.log(fmt.Sprintf("Failed to get history for channel: %s", channel))
				return
			}
			// TODO: merge with existing
			t.history[channel] = history
			t.renderLog()
		}()
	} else {
		// t.markRead(channel)
	}
	t.renderLog()
}

func (t *tight) channelEditor(v *gocui.View, key gocui.Key, ch rune, mod gocui.Modifier) {
	switch {
	// case key == gocui.KeyTab:
	// 	tab = true
	case ch != 0 && mod == 0:
		t.highlightedChannel = 0
		if ch == 'Z' {
			if t.highlightedChannel > 0 {
				t.highlightedChannel -= 1
			}
		} else {
			v.EditWrite(ch)
		}
	case key == gocui.KeySpace:
		v.EditWrite(' ')
	case key == gocui.KeyBackspace || key == gocui.KeyBackspace2:
		v.EditDelete(true)
	case key == gocui.KeyDelete:
		v.EditDelete(false)
	case key == gocui.KeyInsert:
		v.Overwrite = !v.Overwrite
	case key == gocui.KeyEnter:
		// input, _ := v.Line(0)
		c := t.currentChannel
		// if input != "" {

		if t.highlightedChannel < len(t.channels) {
			c = t.channels[t.highlightedChannel]
		}
		// }
		t.selectChannel(c)
		v.Clear()
		fmt.Fprint(v, c)
		return
	case key == gocui.KeyArrowDown:
		if t.highlightedChannel < len(t.channels)-1 {
			t.highlightedChannel += 1
		}

	case key == gocui.KeyCtrlJ:
		if t.highlightedChannel < len(t.channels)-1 {
			t.highlightedChannel += 1
		}
	case key == gocui.KeyTab:
		if t.highlightedChannel < len(t.channels)-1 {
			t.highlightedChannel += 1
		}
	case key == gocui.KeyCtrlK:
		if t.highlightedChannel > 0 {
			t.highlightedChannel -= 1
		}
	case key == gocui.KeyArrowUp:
		if t.highlightedChannel > 0 {
			t.highlightedChannel -= 1
		}
	case key == gocui.KeyArrowLeft:
		v.MoveCursor(-1, 0, false)
	case key == gocui.KeyArrowRight:

		cx, _ := v.Cursor()
		line := v.ViewBuffer()

		if cx < len(line)-1 {
			v.MoveCursor(1, 0, false)
		}

	case key == gocui.KeyCtrlW:
		xCursor, yCursor := v.Cursor()
		word, _ := v.Word(xCursor, yCursor)
		if word == "" {
			word, _ = v.Word(xCursor-1, yCursor)
			v.EditDelete(true)
		}
		for i := 0; i < len(word); i++ {
			v.EditDelete(true)
		}
	case key == gocui.KeyCtrlA:
		v.SetCursor(0, 0)
		v.SetOrigin(0, 0)
	case key == gocui.KeyCtrlK:
		v.Clear()
		v.SetCursor(0, 0)
		v.SetOrigin(0, 0)
	case key == gocui.KeyCtrlE:
		_, yCursor := v.Cursor()
		line, err := v.Line(yCursor)
		if err != nil {
			return
		}
		v.SetCursor(len(line), 0)

	case key == gocui.KeyCtrlU:
		xCursor, _ := v.Cursor()
		for i := 0; i < xCursor; i++ {
			v.EditDelete(true)
		}
	}

	t.printChannelList(v)
}

func (t *tight) printChannelList(v *gocui.View) {
	input, _ := v.Line(0)

	t.channelFilter = input

	sorter := ByFilter(*t)

	sort.Sort(&sorter)

	v.Clear()
	fmt.Fprintf(v, "%s\n", input)

	if input == "" {
		fmt.Fprintf(v, "%s\n", input)
	}

	for i, c := range t.channels {
		format := "%s %d\n"
		if i == t.highlightedChannel {
			format = "> " + format
		} else {
			format = "  " + format
		}
		fmt.Fprintf(v, format, c, t.latestUnread[c])
	}
}

func (t *tight) simpleEditor(v *gocui.View, key gocui.Key, ch rune, mod gocui.Modifier) {
	var tab = false
	var inHistroy = false

	switch {
	case key == gocui.KeyTab:
		tab = true
	case ch != 0 && mod == 0:
		v.EditWrite(ch)
	case key == gocui.KeySpace:
		v.EditWrite(' ')
	case key == gocui.KeyBackspace || key == gocui.KeyBackspace2:
		v.EditDelete(true)
	case key == gocui.KeyDelete:
		v.EditDelete(false)
	case key == gocui.KeyInsert:
		v.Overwrite = !v.Overwrite
	case key == gocui.KeyEnter:

		// if line := v.ViewBuffer(); len(line) > 0 {
		// 	GetLine(Server.Gui, v)
		// } else {
		// 	if c, err := Server.Gui.View(Server.CurrentChannel); err == nil {
		// 		c.Autoscroll = true
		// 	}
		// }

		// InputHistory.Current()
		if mod == gocui.ModAlt {
			v.EditNewLine()
		} else {
			t.SendMsg(v.Buffer())
			v.Clear()
			v.SetCursor(0, 0)
		}

		// v.Rewind()

	// case key == gocui.MouseMiddle:
	// nextView(Server.Gui, v)
	// case key == gocui.MouseRight:

	case key == gocui.KeyArrowDown:
		// inHistroy = true

		// if line := InputHistory.Next(); len(line) > 0 {
		// 	v.Clear()
		// 	v.SetCursor(0, 0)
		// 	v.SetOrigin(0, 0)

		// 	fmt.Fprint(v, line)
		// 	v.SetCursor(len(v.Buffer())-1, 0)
		// }
	case key == gocui.KeyArrowUp:
		// inHistroy = true

		// if line := InputHistory.Prev(); len(line) > 0 {
		// 	v.Clear()
		// 	v.SetCursor(0, 0)
		// 	v.SetOrigin(0, 0)

		// 	fmt.Fprint(v, line)
		// 	v.SetCursor(len(v.Buffer())-1, 0)
		// }
	case key == gocui.KeyArrowLeft:
		v.MoveCursor(-1, 0, false)
	case key == gocui.KeyArrowRight:

		cx, _ := v.Cursor()
		line := v.ViewBuffer()

		// logger.Logger.Println(len(line), cx)
		// logger.Logger.Println(spew.Sdump(line))

		// if cx == 0 {
		// v.MoveCursor(-1, 0, false)
		if cx < len(line)-1 {
			v.MoveCursor(1, 0, false)
		}

	case key == gocui.KeyCtrlW:
		xCursor, yCursor := v.Cursor()
		word, _ := v.Word(xCursor, yCursor)
		if word == "" {
			word, _ = v.Word(xCursor-1, yCursor)
			v.EditDelete(true)
		}
		for i := 0; i < len(word); i++ {
			v.EditDelete(true)
		}

		// v.SetCursor(0, 0)
		// v.SetOrigin(0, 0)
	case key == gocui.KeyCtrlA:
		v.SetCursor(0, 0)
		v.SetOrigin(0, 0)
	case key == gocui.KeyCtrlK:
		v.Clear()
		v.SetCursor(0, 0)
		v.SetOrigin(0, 0)
	case key == gocui.KeyCtrlE:
		_, yCursor := v.Cursor()
		line, err := v.Line(yCursor)
		if err != nil {
			return
		}
		v.SetCursor(len(line), 0)

	case key == gocui.KeyCtrlU:
		xCursor, _ := v.Cursor()
		for i := 0; i < xCursor; i++ {
			v.EditDelete(true)
		}
	}

	if !inHistroy {
		// InputHistory.Current()
	}

	if !tab {
		// logger.Logger.Print("CALL\n")

		// inCacheTab = false
		// cacheTabSearch = ""
		// cacheTabResults = []string{}
	}
}

func (t *tight) setCurrentViewOnTop(g *gocui.Gui) error {
	_, err := g.SetCurrentView(t.view)
	if err != nil {
		return err
	}
	v, err := g.SetViewOnTop(t.view)
	if err != nil {
		return err
	}

	if t.view == "channels" {
		v.Clear()
		v.SetCursor(0, 0)
		t.channelFilter = ""
		t.highlightedChannel = 0
		t.printChannelList(v)
	}
	return nil
}

func (t *tight) channelPicker(g *gocui.Gui, v *gocui.View) error {
	t.view = "channels"
	return nil
}

func (t *tight) normal(g *gocui.Gui, v *gocui.View) error {
	t.view = "input"
	return nil
}

func (t *tight) GetUserInfo(userID string) *slack.User {
	u := t.Users.ByID(userID)
	if u == nil {
		return nil
		// return &slack.User{}
		// log.Warningf("Unknown user ID '%s'", userID)
	}
	return u
}

// ExpandUserIds will convert slack user tags with user's nicknames
func (t *tight) ExpandUserIds(text string) string {
	return rxSlackUser.ReplaceAllStringFunc(text, func(subs string) string {
		uid := subs[2 : len(subs)-1]
		user := t.GetUserInfo(uid)
		if user == nil {
			return subs
		}
		return fmt.Sprintf("@%s", user.Name)
	})
}

// ExpandText expands and unquotes text and URLs from Slack's messages. Slack
// quotes the text and URLS, and the latter are enclosed in < and >. It also
// translates potential URLs into actual URLs (e.g. when you type "example.com"),
// so you will get something like <http://example.com|example.com>. This
// function tries to detect them and unquote and expand them for a better
// visualization on IRC.
func (t *tight) ExpandText(text string) string {
	text = rxSlackUrls.ReplaceAllStringFunc(text, func(subs string) string {
		if !strings.HasPrefix(subs, "<") && !strings.HasSuffix(subs, ">") {
			return subs
		}

		// Slack URLs may contain an URL followed by a "|", followed by the
		// original message. Detect the pipe and only parse the URL.
		var (
			slackURL = subs[1 : len(subs)-1]
			slackMsg string
		)
		idx := strings.LastIndex(slackURL, "|")
		if idx >= 0 {
			slackMsg = slackURL[idx+1:]
			slackURL = slackURL[:idx]
		}

		u, err := url.Parse(slackURL)
		if err != nil {
			return subs
		}
		// Slack escapes the URLs passed by the users, let's undo that
		//u.RawQuery = html.UnescapeString(u.RawQuery)
		if slackMsg == "" {
			return u.String()
		}
		return fmt.Sprintf("%s (%s)", slackMsg, u.String())
	})
	text = html.UnescapeString(text)
	return text

}

func parseMentions(text string) string {
	tokens := strings.Split(text, " ")
	for idx, token := range tokens {
		if token == "@here" {
			tokens[idx] = "<!here>"
		} else if token == "@channel" {
			tokens[idx] = "<!channel>"
		} else if token == "@everyone" {
			tokens[idx] = "<!everyone>"
		} else if strings.HasPrefix(token, "@") {
			tokens[idx] = "<" + token + ">"
		}
	}
	return strings.Join(tokens, " ")
}

func (t *tight) SendMsg(text string) {
	if text == "" {
		return
	}

	if strings.HasPrefix(text, "/") {
		command := strings.Split(text, " ")
		switch command[0] {
		case "/r":
			if len(command) == 2 {
				t.selectChannel(fmt.Sprintf(">%s-%s", t.currentChannel, command[1]))
			}
			return
		}
		return
	}

	text = parseMentions(text)

	opts := []slack.MsgOption{}
	opts = append(opts, slack.MsgOptionAsUser(true))
	opts = append(opts, slack.MsgOptionText(strings.TrimSpace(text), false))

	target, targetTs, err := t.getChannelIdFromName(t.currentChannel)
	if err != nil {
		t.log(fmt.Sprintf("Failed to get channel ID from name %s: %v", t.currentChannel, err))
		return
	}

	if targetTs != "" {
		opts = append(opts, slack.MsgOptionTS(targetTs))
	}

	channel, err := t.GetConversationInfo(target)
	if err != nil {
		t.log(fmt.Sprintf("Failed to get channel info %s: %v", target, err))
		return
	}
	if _, _, err := t.SlackClient.PostMessage(channel.ID, opts...); err != nil {
		t.log(fmt.Sprintf("Failed to post message to Slack to target %s: %v", t.currentChannel, err))
	}
}

func quit(g *gocui.Gui, v *gocui.View) error {
	return gocui.ErrQuit
}
func Min(x, y int) int {
	if x < y {
		return x
	}
	return y
}
