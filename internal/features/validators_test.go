package features_test

// DONE: PRD 6.5 (shared feature validators) / STANDARDS A, F

// Per-feature validator tests. The features register themselves in
// init() — but other tests in this binary call features.ResetForTest
// which wipes the registry, so we reset and re-register at the start
// of every validator test.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/davinakmalyasha/agents-proxy/internal/features"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/article"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/booking"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/menu"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/promo"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/review"
)

func reregister() {
	features.ResetForTest()
	menu.Register()
	booking.Register()
	review.Register()
	article.Register()
	promo.Register()
}

func TestMenuValidator(t *testing.T) {
	reregister()
	f, ok := features.Get("menu")
	if !ok {
		t.Fatal("menu feature not registered")
	}
	if f.Kind != "menu_item" {
		t.Errorf("kind=%s want menu_item", f.Kind)
	}
	cases := []struct {
		name  string
		body  string
		valid bool
	}{
		{"happy", `{"name":"Nasi","price":15000}`, true},
		{"happy_zero_price", `{"name":"Sambal","price":0}`, true},
		{"missing_name", `{"price":100}`, false},
		{"negative_price", `{"name":"x","price":-1}`, false},
		{"empty_name", `{"name":"","price":1}`, false},
		{"whitespace_name", `{"name":"   ","price":1}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := f.ValidatePayload(json.RawMessage(c.body))
			if (err == nil) != c.valid {
				t.Errorf("ValidatePayload(%s) err=%v valid=%v", c.body, err, c.valid)
			}
		})
	}
}

func TestBookingValidator(t *testing.T) {
	reregister()
	f, ok := features.Get("booking")
	if !ok {
		t.Fatal("booking feature not registered")
	}
	if f.Kind != "booking" {
		t.Errorf("kind=%s want booking", f.Kind)
	}
	cases := []struct {
		name  string
		body  string
		valid bool
	}{
		{"happy", `{"name":"Budi","date":"2026-09-15"}`, true},
		{"datetime_alt", `{"name":"Budi","datetime":"2026-09-15T10:00:00Z"}`, true},
		{"missing_name", `{"date":"2026-09-15"}`, false},
		{"missing_date", `{"name":"x"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := f.ValidatePayload(json.RawMessage(c.body))
			if (err == nil) != c.valid {
				t.Errorf("ValidatePayload(%s) err=%v valid=%v", c.body, err, c.valid)
			}
			if !c.valid && err != nil && !strings.Contains(err.Error(), "required") {
				t.Errorf("error message should contain 'required': %v", err)
			}
		})
	}
}

func TestReviewValidator(t *testing.T) {
	reregister()
	f, ok := features.Get("review")
	if !ok {
		t.Fatal("review feature not registered")
	}
	cases := []struct {
		body  string
		valid bool
	}{
		{`{"rating":3,"author":"Budi"}`, true},
		{`{"rating":1,"author":"x"}`, true},
		{`{"rating":5,"author":"x"}`, true},
		{`{"rating":0,"author":"x"}`, false},
		{`{"rating":6,"author":"x"}`, false},
		{`{"rating":3,"author":""}`, false},
		{`{"rating":3}`, false},
	}
	for _, c := range cases {
		err := f.ValidatePayload(json.RawMessage(c.body))
		if (err == nil) != c.valid {
			t.Errorf("ValidatePayload(%s) err=%v valid=%v", c.body, err, c.valid)
		}
	}
}

func TestArticleValidator(t *testing.T) {
	reregister()
	f, ok := features.Get("article")
	if !ok {
		t.Fatal("article feature not registered")
	}
	cases := []struct {
		body  string
		valid bool
	}{
		{`{"title":"Hello","body":"World"}`, true},
		{`{"title":"x","body":""}`, false},
		{`{"title":"","body":"x"}`, false},
		{`{"body":"x"}`, false},
	}
	for _, c := range cases {
		err := f.ValidatePayload(json.RawMessage(c.body))
		if (err == nil) != c.valid {
			t.Errorf("ValidatePayload(%s) err=%v valid=%v", c.body, err, c.valid)
		}
	}
}

func TestPromoValidator(t *testing.T) {
	reregister()
	f, ok := features.Get("promo")
	if !ok {
		t.Fatal("promo feature not registered")
	}
	cases := []struct {
		body  string
		valid bool
	}{
		{`{"title":"Diskon 50%","discount":0.5}`, true},
		{`{"title":"Free","discount":0}`, true},
		{`{"title":"","discount":0.1}`, false},
		{`{"title":"x","discount":-1}`, false},
		{`{"discount":0.1}`, false},
	}
	for _, c := range cases {
		err := f.ValidatePayload(json.RawMessage(c.body))
		if (err == nil) != c.valid {
			t.Errorf("ValidatePayload(%s) err=%v valid=%v", c.body, err, c.valid)
		}
	}
}
