// Package telegrambot serves read-only operational commands over Telegram.
//
// Read-only is a security boundary, not a scope shortcut. The bot token lives
// in .env and travels to Telegram's servers on every poll; anyone who obtains
// it can speak to this bot. Every command here therefore answers questions and
// changes nothing, so the worst outcome of a leaked token is disclosure of
// operational metrics — never a restarted container or a closed room.
//
// Transport is long polling rather than a webhook: a webhook needs a public
// HTTPS route punched through the gateway, which is a new inbound attack
// surface and one more nginx location to get wrong. Long polling makes only
// outbound calls.
package telegrambot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// pollTimeout is the long-poll hold time. Telegram keeps the request open
	// this long waiting for an update, so the loop costs one idle connection
	// rather than a request every second.
	pollTimeout = 50
	apiBase     = "https://api.telegram.org"
)

type Config struct {
	BotToken string
	// ChatID is the only chat the bot answers in. A message from anywhere else
	// is ignored outright — the bot is discoverable by username, so without
	// this check any stranger could query production metrics.
	ChatID   string
	ThreadID string
	// AdminUserIDs optionally narrows further, to specific Telegram accounts
	// inside that chat. Empty means "anyone in the allowed chat".
	AdminUserIDs map[int64]bool
}

type bot struct {
	cfg    Config
	client *http.Client
	offset int64
	// startedAt backs /status uptime, which is the quickest way to spot a
	// container that has been restarting behind your back.
	startedAt time.Time
}

type update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		MessageID int64  `json:"message_id"`
		Date      int64  `json:"date"`
		Text      string `json:"text"`
		// MessageThreadID is the forum topic the command was typed in. Absent
		// for the General topic and for non-forum groups.
		MessageThreadID int64 `json:"message_thread_id"`
		IsTopicMessage  bool  `json:"is_topic_message"`
		Chat            struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		From *struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
		} `json:"from"`
	} `json:"message"`
}

// Start launches the polling loop. It is a no-op without credentials, so dev
// and CI run unchanged.
func Start(cfg Config) {
	if strings.TrimSpace(cfg.BotToken) == "" || strings.TrimSpace(cfg.ChatID) == "" {
		log.Println("[telegrambot] disabled: TELEGRAM_BOT_TOKEN or TELEGRAM_CHAT_ID not set.")
		return
	}
	b := &bot{
		cfg:       cfg,
		client:    &http.Client{Timeout: (pollTimeout + 10) * time.Second},
		startedAt: time.Now(),
	}
	log.Println("[telegrambot] read-only command bot starting (long polling)...")
	go b.run()
}

func (b *bot) run() {
	b.registerCommands()
	b.skipBacklog()
	for {
		ups, err := b.getUpdates()
		if err != nil {
			// A network blip must not spin the loop; back off and retry.
			log.Printf("[telegrambot] getUpdates failed: %v", err)
			time.Sleep(10 * time.Second)
			continue
		}
		for _, u := range ups {
			b.offset = u.UpdateID + 1
			b.handle(u)
		}
	}
}

// registerCommands publishes the command menu (the list Telegram autocompletes
// when you type "/", and what the blue Menu button shows). Without this call
// the bot still answers every command — it just looks like it has none.
//
// Scoped to the configured chat rather than set globally: the bot is
// discoverable by username, and a global menu would advertise production
// operations commands to anyone who opens a DM with it.
func (b *bot) registerCommands() {
	list := make([]map[string]string, 0, len(commands))
	for _, c := range commands {
		list = append(list, map[string]string{"command": c.Name, "description": c.Menu})
	}
	cmdJSON, err := json.Marshal(list)
	if err != nil {
		log.Printf("[telegrambot] could not encode command list: %v", err)
		return
	}
	scope, err := json.Marshal(map[string]interface{}{"type": "chat", "chat_id": b.cfg.ChatID})
	if err != nil {
		log.Printf("[telegrambot] could not encode command scope: %v", err)
		return
	}
	if _, err := b.call("setMyCommands", url.Values{
		"commands": {string(cmdJSON)},
		"scope":    {string(scope)},
	}); err != nil {
		// Not fatal: the commands keep working, they are just not advertised.
		log.Printf("[telegrambot] setMyCommands failed: %v", err)
		return
	}
	log.Printf("[telegrambot] registered %d commands for chat %s", len(commands), b.cfg.ChatID)
}

// skipBacklog discards updates queued while the process was down. Telegram
// holds them for 24h, so without this a restart would replay every command
// typed overnight — including a /mute whose window has long since passed.
func (b *bot) skipBacklog() {
	ups, err := b.call("getUpdates", url.Values{"offset": {"-1"}, "limit": {"1"}})
	if err != nil {
		return
	}
	var parsed []update
	if json.Unmarshal(ups, &parsed) == nil && len(parsed) > 0 {
		b.offset = parsed[0].UpdateID + 1
	}
}

func (b *bot) getUpdates() ([]update, error) {
	raw, err := b.call("getUpdates", url.Values{
		"offset":          {fmt.Sprint(b.offset)},
		"timeout":         {fmt.Sprint(pollTimeout)},
		"allowed_updates": {`["message"]`},
	})
	if err != nil {
		return nil, err
	}
	var parsed []update
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode updates: %w", err)
	}
	return parsed, nil
}

// call performs one Bot API request and unwraps the {"ok":…,"result":…} envelope.
func (b *bot) call(method string, params url.Values) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), (pollTimeout+10)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/bot%s/%s", apiBase, b.cfg.BotToken, method),
		strings.NewReader(params.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var env struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode envelope: %w", err)
	}
	if !env.OK {
		return nil, fmt.Errorf("telegram: %s", env.Description)
	}
	return env.Result, nil
}

func (b *bot) handle(u update) {
	m := u.Message
	if m == nil || m.Text == "" {
		return
	}
	if fmt.Sprint(m.Chat.ID) != strings.TrimSpace(b.cfg.ChatID) {
		return // not our chat; stay silent rather than confirm the bot exists
	}
	if len(b.cfg.AdminUserIDs) > 0 && (m.From == nil || !b.cfg.AdminUserIDs[m.From.ID]) {
		b.reply("⛔ Tài khoản này không nằm trong TELEGRAM_ADMIN_IDS.")
		return
	}
	// Ignore anything that sat in the queue: a stale command is at best
	// confusing and at worst acts on a situation that has already changed.
	if time.Since(time.Unix(m.Date, 0)) > 2*time.Minute {
		return
	}

	text := strings.TrimSpace(m.Text)
	if !strings.HasPrefix(text, "/") {
		return
	}
	// Telegram appends @botname when several bots share a group.
	fields := strings.Fields(text)
	cmd := strings.ToLower(fields[0])
	if i := strings.Index(cmd, "@"); i > 0 {
		cmd = cmd[:i]
	}
	args := fields[1:]

	log.Printf("[telegrambot] command %s from chat %d topic %d", cmd, m.Chat.ID, m.MessageThreadID)

	// Answer in the topic the command came from, not in the alerts topic.
	// Commands typed in General are NOT rejected — filtering by topic would
	// make the bot look dead to anyone who asked in the wrong place, and the
	// chat-id check is what actually provides the access control.
	thread := b.cfg.ThreadID
	if m.IsTopicMessage && m.MessageThreadID != 0 {
		thread = fmt.Sprint(m.MessageThreadID)
	}
	b.replyIn(thread, dispatch(cmd, args, b.startedAt))
}

func (b *bot) reply(text string) { b.replyIn(b.cfg.ThreadID, text) }

func (b *bot) replyIn(threadID, text string) {
	params := url.Values{
		"chat_id":                  {b.cfg.ChatID},
		"text":                     {text},
		"parse_mode":               {"HTML"},
		"disable_web_page_preview": {"true"},
	}
	if threadID != "" {
		params.Set("message_thread_id", threadID)
	}
	if _, err := b.call("sendMessage", params); err != nil {
		log.Printf("[telegrambot] reply failed: %v", err)
	}
}
