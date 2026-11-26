package main

import "testing"

func TestParseServerTiming(t *testing.T) {
	cases := map[string][3]float64{
		"app;dur=12.345":                                {12.345, 0, 0},
		"edge;dur=1, app;dur=7.5":                       {7.5, 0, 0},
		"app;dur=12.795, db;dur=3.210, redis;dur=1.050": {12.795, 3.21, 1.05},
		"app;dur=10, db;dur=0, redis;dur=0":             {10, 0, 0},
		"":                                              {0, 0, 0},
		"cache;desc=hit":                                {0, 0, 0},
	}
	for header, want := range cases {
		app, db, redis := parseServerTiming(header)
		got := [3]float64{app, db, redis}
		if got != want {
			t.Errorf("parseServerTiming(%q) = %v, want %v", header, got, want)
		}
	}
}

func TestInstanceTrackerColdOnce(t *testing.T) {
	tr := newInstanceTracker()
	if !tr.observe("a") {
		t.Fatal("first request for instance a should be cold")
	}
	if tr.observe("a") {
		t.Fatal("second request for instance a should be warm")
	}
	if !tr.observe("b") {
		t.Fatal("first request for instance b should be cold")
	}
	if tr.observe("") {
		t.Fatal("empty instance must not be classified as cold")
	}
	if tr.count() != 2 {
		t.Fatalf("instances = %d, want 2", tr.count())
	}
}
