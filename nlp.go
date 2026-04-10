package main

import (
	"regexp"
	"strings"

	"github.com/kljensen/snowball"
)

var punctRe = regexp.MustCompile(`[^\p{L}\p{N}\s]+`)

func stripPunct(text string) string {
	return punctRe.ReplaceAllString(text, " ")
}

func stemWord(word string) string {
	s, err := snowball.Stem(word, "russian", true)
	if err != nil || s == "" {
		return word
	}
	return s
}

// buildSearchVector produces a stemmed, punctuation-free string for FTS5 indexing.
// Section labels from the AI response ("Описание:", "Текст:", "Персоны:") are stripped
// so they don't match every search query.
func buildSearchVector(text string) string {
	text = strings.NewReplacer(
		"Описание:", "",
		"Текст:", "",
		"Персоны:", "",
		"Текст отсутствует", "",
	).Replace(text)
	text = strings.ToLower(stripPunct(text))
	words := strings.Fields(text)
	for i, w := range words {
		words[i] = stemWord(w)
	}
	return strings.Join(words, " ")
}

// buildFTSQuery turns a user query into an FTS5 MATCH expression:
// "котики кушают рыбку" → "котик* AND кушат* AND рыбк*"
func buildFTSQuery(query string) string {
	query = strings.ToLower(stripPunct(query))
	words := strings.Fields(query)
	if len(words) == 0 {
		return ""
	}
	terms := make([]string, 0, len(words))
	for _, w := range words {
		terms = append(terms, stemWord(w)+"*")
	}
	return strings.Join(terms, " AND ")
}
