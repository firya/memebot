package main

import (
	"strings"
	"testing"
)

func TestStripPunct(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"hello, world!", "hello  world "},
		{"котики!!!", "котики "},
		{"no-punct", "no punct"},
		{"привет.мир", "привет мир"},
		{"чисто слова", "чисто слова"},
	}
	for _, tc := range tests {
		got := stripPunct(tc.in)
		if got != tc.want {
			t.Errorf("stripPunct(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestStemWord(t *testing.T) {
	// Snowball should reduce Russian words to their stems.
	tests := []struct {
		in   string
		want string // stem prefix that the result should contain
	}{
		{"котики", "кот"},
		{"кушают", "куша"},
		{"рыбку", "рыб"},
	}
	for _, tc := range tests {
		got := stemWord(tc.in)
		if !strings.HasPrefix(got, tc.want) {
			t.Errorf("stemWord(%q) = %q, expected prefix %q", tc.in, got, tc.want)
		}
	}
}

func TestStemWordFallback(t *testing.T) {
	// Non-Russian words should be returned unchanged (snowball falls back).
	got := stemWord("hello")
	if got == "" {
		t.Errorf("stemWord(\"hello\") returned empty string, want non-empty")
	}
}

func TestBuildSearchVector(t *testing.T) {
	tests := []struct {
		desc     string
		in       string
		contains string
		absent   string
	}{
		{
			desc:     "strips section labels",
			in:       "Описание: кот сидит. Текст: мяу. Персоны: Нет.",
			absent:   "описани",
			contains: "кот",
		},
		{
			desc:   "strips Текст отсутствует",
			in:     "Текст отсутствует",
			absent: "отсутству",
		},
		{
			desc:     "stems words",
			in:       "Описание: котики кушают рыбку",
			contains: "кот",
		},
		{
			desc:     "lowercases",
			in:       "Описание: КОТИК",
			contains: "кот",
			absent:   "КОТИК",
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			got := buildSearchVector(tc.in)
			if tc.contains != "" && !strings.Contains(got, tc.contains) {
				t.Errorf("buildSearchVector(%q) = %q, expected to contain %q", tc.in, got, tc.contains)
			}
			if tc.absent != "" && strings.Contains(got, tc.absent) {
				t.Errorf("buildSearchVector(%q) = %q, expected NOT to contain %q", tc.in, got, tc.absent)
			}
		})
	}
}

func TestBuildFTSQuery(t *testing.T) {
	tests := []struct {
		desc     string
		in       string
		want     string
		contains string
	}{
		{
			desc: "empty input",
			in:   "",
			want: "",
		},
		{
			desc:     "single word stemmed",
			in:       "котики",
			contains: "кот",
		},
		{
			desc:     "multiple words joined with AND",
			in:       "котики рыбку",
			contains: " AND ",
		},
		{
			desc:     "punctuation stripped",
			in:       "котики!!!",
			contains: "кот",
		},
		{
			desc:     "each term ends with wildcard",
			in:       "кот",
			contains: "*",
		},
		{
			desc:     "whitespace only",
			in:       "   ",
			want:     "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			got := buildFTSQuery(tc.in)
			if tc.want != "" && got != tc.want {
				t.Errorf("buildFTSQuery(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if tc.want == "" && tc.contains == "" && got != "" {
				t.Errorf("buildFTSQuery(%q) = %q, want empty", tc.in, got)
			}
			if tc.contains != "" && !strings.Contains(got, tc.contains) {
				t.Errorf("buildFTSQuery(%q) = %q, expected to contain %q", tc.in, got, tc.contains)
			}
		})
	}
}
