package router

import (
	"html/template"
	"io/fs"
	"strings"
	"testing"

	"github.com/v03413/bepusdt/static"
)

func TestLangGeCheckoutTemplateIsEmbedded(t *testing.T) {
	checkout, err := readCheckoutInfoFromFS(static.Checkout, "checkout/langge")
	if err != nil {
		t.Fatalf("read langge checkout info: %v", err)
	}

	if checkout.Name != "LangGe design" {
		t.Fatalf("unexpected checkout name: %q", checkout.Name)
	}
	if checkout.Author == "" {
		t.Fatal("checkout author is required")
	}
	if checkout.Desc == "" {
		t.Fatal("checkout desc is required")
	}

	view, err := fs.ReadFile(static.Checkout, "checkout/langge/views/checkout.html")
	if err != nil {
		t.Fatalf("read langge checkout template: %v", err)
	}
	if !strings.Contains(string(view), "{{ .trade_id }}") {
		t.Fatal("langge checkout template must only depend on trade_id server injection")
	}

	tmpl := template.New("default")
	if !registerTemplatesFromFS(tmpl, static.Checkout, "checkout/langge", "langge") {
		t.Fatal("langge checkout template was not registered")
	}
	if tmpl.Lookup("langge/checkout.html") == nil {
		t.Fatal("langge checkout template was not registered under expected name")
	}
}

func TestOfficialCheckoutTransactionHashVerificationControlsAreEmbedded(t *testing.T) {
	view, err := fs.ReadFile(static.Checkout, "checkout/official/views/checkout.html")
	if err != nil {
		t.Fatalf("read official checkout template: %v", err)
	}
	script, err := fs.ReadFile(static.Checkout, "checkout/official/assets/js/checkout.js")
	if err != nil {
		t.Fatalf("read official checkout script: %v", err)
	}
	css, err := fs.ReadFile(static.Checkout, "checkout/official/assets/css/checkout.css")
	if err != nil {
		t.Fatalf("read official checkout stylesheet: %v", err)
	}

	for _, file := range []struct {
		name     string
		content  []byte
		required []string
	}{
		{
			name:    "template",
			content: view,
			required: []string{
				`id="transactionHashForm"`,
				`id="transactionHashInput"`,
				`id="transactionHashSubmit"`,
				`style="display:none;"`,
			},
		},
		{
			name:    "script",
			content: script,
			required: []string{
				"can_verify_transaction_hash",
				"error_code",
				"verify-transaction",
				"showTimeout",
				"createTransactionHashForm",
			},
		},
		{
			name:    "stylesheet",
			content: css,
			required: []string{
				".transaction-hash-form",
				".transaction-hash-input",
			},
		},
	} {
		for _, expected := range file.required {
			if !strings.Contains(string(file.content), expected) {
				t.Fatalf("official checkout %s is missing %q", file.name, expected)
			}
		}
	}

	js := string(script)
	for _, expected := range []string{
		"can_verify_transaction_hash: d.can_verify_transaction_hash === true",
		"if (canVerifyTransactionHash()) {",
		"createTransactionHashForm('timeoutTransactionHashForm')",
	} {
		if !strings.Contains(js, expected) {
			t.Fatalf("official checkout script is missing the capability-controlled verification path %q", expected)
		}
	}
	if strings.Contains(js, "usdt.solana") || strings.Contains(js, "usdc.solana") || strings.Contains(js, "usdt.bep20") {
		t.Fatal("official checkout must use the backend capability flag instead of a frontend transaction-hash chain allowlist")
	}

	for _, locale := range []string{"zh", "en"} {
		content, err := fs.ReadFile(static.Checkout, "checkout/official/assets/locales/"+locale+".json")
		if err != nil {
			t.Fatalf("read %s checkout locale: %v", locale, err)
		}
		for _, errorCode := range []string{
			"invalid_hash",
			"unsupported_network",
			"transaction_not_found",
			"transaction_mismatch",
			"transaction_already_used",
			"order_not_receivable",
			"verification_unavailable",
		} {
			if !strings.Contains(string(content), `"`+errorCode+`"`) {
				t.Fatalf("%s checkout locale is missing transaction hash error code %q", locale, errorCode)
			}
		}
	}
}
