# Tight

Slack TUI client

# Build

    cd cmd/tight
    go build

# Usage

    TOKEN=xoxp-... ./tight

Press CTRL+p to open a channel list. Use `tab`, `shift+tab`, `ctrl+k`, `ctrl+j` or arrow keys to navigate the list of channels. Start typing channel name to filter the list. The list is sorted by number of unread messages, channel with most unread messages is on top.

Threads will appear as separate channels in the channel list. They contain the name of the channel and timestamp of the first message in the thread.

Token can be obtained by following [these instructions](https://github.com/insomniacslk/irc-slack#authentication).

# Features

- [x] Faster than Slack browser client
- [x] No images
- [x] View and reply to threads
- [x] Mentions
- [x] Show reactions
- [ ] Slash commands
- [ ] Allow starting threads
- [ ] Allow reacting to messages
- [ ] Scroll back further in the past than initial 100 messages

# Prior art

Inspired by [irc-slack](https://github.com/insomniacslk/irc-slack) gateway
