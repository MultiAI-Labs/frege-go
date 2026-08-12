// Command telegrambot is a minimal Telegram bot that runs one Frege tool for
// every message it receives and replies with the result.
//
// It has no third-party dependencies: it talks to the Telegram Bot API over
// plain HTTP and to Frege through the SDK. It is meant to be read, not shipped
// as-is.
//
// It authenticates to Frege with an API KEY — a static credential minted in the
// dashboard (Project → API keys). No human sign-in, no token to refresh: the key
// is the whole story.
//
// Environment:
//
//	TELEGRAM_BOT_TOKEN   required — from @BotFather
//	FREGE_API_KEY        required — frege_sk_… minted in the dashboard
//	FREGE_PROJECT_ID     required — numeric project id (see the dashboard URL)
//	FREGE_TOOL           required — tool to run, e.g. get_account_profile
//	FREGE_ARG            optional — send the message text under this argument
//	FREGE_BASE_URL       optional — default https://frege.io
//
// Send the bot any text to run FREGE_TOOL; send /tools to list the project's
// tools.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	frege "github.com/MultiAI-Labs/frege-go"
)

func main() {
	botToken := must("TELEGRAM_BOT_TOKEN")
	base := os.Getenv("FREGE_BASE_URL")
	toolName := must("FREGE_TOOL")
	argName := os.Getenv("FREGE_ARG")

	projectID, err := strconv.ParseInt(must("FREGE_PROJECT_ID"), 10, 64)
	if err != nil {
		log.Fatalf("FREGE_PROJECT_ID must be a number: %v", err)
	}

	// An API key is a static bearer, so this is the entire auth setup. No sign-in,
	// no refresh, no token to persist.
	fc := frege.New(frege.StaticToken(must("FREGE_API_KEY")), frege.WithBaseURL(base))

	bot := &telegram{token: botToken, http: &http.Client{Timeout: 65 * time.Second}}
	log.Printf("bot up; running tool %q on project %d for each message", toolName, projectID)

	var offset int64
	for {
		updates, err := bot.getUpdates(offset)
		if err != nil {
			log.Printf("getUpdates: %v", err)
			time.Sleep(3 * time.Second)
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			if u.Message == nil || strings.TrimSpace(u.Message.Text) == "" {
				continue
			}
			reply := handle(fc, projectID, toolName, argName, strings.TrimSpace(u.Message.Text))
			if err := bot.send(u.Message.Chat.ID, reply); err != nil {
				log.Printf("sendMessage: %v", err)
			}
		}
	}
}

// handle turns one incoming message into a reply string.
func handle(fc *frege.Client, projectID int64, toolName, argName, text string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	if text == "/tools" || text == "/start" {
		ops, err := fc.ListOperations(ctx, projectID)
		if err != nil {
			return "Could not list tools: " + err.Error()
		}
		var b strings.Builder
		b.WriteString("Tools on this project:\n")
		for _, op := range ops {
			fmt.Fprintf(&b, "\n• %s — %s", op.ToolName, op.Summary)
		}
		return b.String()
	}

	args := map[string]any{}
	if argName != "" {
		args[argName] = text
	}
	res, err := fc.InvokeTool(ctx, projectID, toolName, args)
	if err != nil {
		return "The tool call failed: " + err.Error()
	}
	// StatusCode/Body are the upstream's own response.
	body := res.Body
	if len(body) > 3500 {
		body = body[:3500] + "\n…(truncated)"
	}
	return fmt.Sprintf("%s → %d\n%s", res.ToolName, res.StatusCode, body)
}

// ---- the tiny bit of Telegram Bot API we use ------------------------------

type telegram struct {
	token string
	http  *http.Client
}

type tgUpdate struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		Text string `json:"text"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	} `json:"message"`
}

func (t *telegram) getUpdates(offset int64) ([]tgUpdate, error) {
	u := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?timeout=30&offset=%d", url.PathEscape(t.token), offset)
	resp, err := t.http.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		OK     bool       `json:"ok"`
		Result []tgUpdate `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if !out.OK {
		return nil, fmt.Errorf("telegram getUpdates returned ok=false")
	}
	return out.Result, nil
}

func (t *telegram) send(chatID int64, text string) error {
	payload, _ := json.Marshal(map[string]any{"chat_id": chatID, "text": text})
	u := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", url.PathEscape(t.token))
	resp, err := t.http.Post(u, "application/json", strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func must(key string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		log.Fatalf("%s is required", key)
	}
	return v
}
