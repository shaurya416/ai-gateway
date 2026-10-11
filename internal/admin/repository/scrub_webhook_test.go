package repository

import (
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
)

// TestScrubConfigSecrets_WebhookURLPath: chat and incident tools put a
// webhook's token in its PATH, which the general URL rule — userinfo and
// query values only — would serve whole to any read-only caller.
func TestScrubConfigSecrets_WebhookURLPath(t *testing.T) {
	const token = "T0000/B0000/XXXXXXXXXXXXXXXXXXXXXXXX"
	live := config.Config{Notifications: &config.NotificationsConfig{Webhooks: []config.WebhookConfig{
		{Name: "chat", URL: "https://hooks.example.com/services/" + token, Events: []string{config.WebhookEventCircuitOpened}},
		{Name: "basic", URL: "https://user:pw@alerts.example.com/hook?sig=abc", Events: []string{config.WebhookEventCircuitOpened}},
		{Name: "ref", URL: "${CHAT_WEBHOOK_URL}", Secret: "${CHAT_WEBHOOK_SECRET}", Events: []string{config.WebhookEventCircuitOpened}},
		{Name: "literal-secret", URL: "https://h/x", Secret: "not-a-reference", Events: []string{config.WebhookEventCircuitOpened}},
	}}}

	got := ScrubConfigSecrets(live).Notifications.Webhooks

	if strings.Contains(got[0].URL, "XXXXXXXX") || got[0].URL != "https://hooks.example.com/"+redactedPlaceholder {
		t.Fatalf("webhook path served: %q", got[0].URL)
	}
	if strings.Contains(got[1].URL, "user") || strings.Contains(got[1].URL, "pw") || strings.Contains(got[1].URL, "sig") {
		t.Fatalf("webhook userinfo or query served: %q", got[1].URL)
	}
	if !strings.Contains(got[1].URL, "alerts.example.com") {
		t.Fatalf("host should stay legible, got %q", got[1].URL)
	}
	if got[2].URL != "${CHAT_WEBHOOK_URL}" || got[2].Secret != "${CHAT_WEBHOOK_SECRET}" {
		t.Fatalf("references must survive: %+v", got[2])
	}
	if got[3].Secret != redactedPlaceholder {
		t.Fatalf("a literal secret was served: %q", got[3].Secret)
	}
	if got[0].Name != "chat" || got[0].Events[0] != config.WebhookEventCircuitOpened {
		t.Fatalf("settings should stay legible: %+v", got[0])
	}
	if live.Notifications.Webhooks[0].URL != "https://hooks.example.com/services/"+token {
		t.Fatal("live config mutated")
	}
	// Whatever is withheld on the way out is refused on the way back in.
	if field := RedactedSecretField(ScrubConfigSecrets(live)); field != "notifications.webhooks[0].url" {
		t.Fatalf("RedactedSecretField = %q, want notifications.webhooks[0].url", field)
	}
}
