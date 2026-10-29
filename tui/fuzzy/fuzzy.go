package fuzzy

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Match is the result of comparing a query against text.
// Lower scores are better, matching Pi's tui fuzzy scorer.
type Match struct {
	Matches bool
	Score   float64
}

var (
	alphaNumericRe = regexp.MustCompile(`^([a-z]+)([0-9]+)$`)
	numericAlphaRe = regexp.MustCompile(`^([0-9]+)([a-z]+)$`)
)

// MatchString reports whether query's characters occur in text in order and
// returns a quality score. Matching is case-insensitive. Lower scores are
// better. It mirrors Pi's tui/src/fuzzy.ts scoring, including the swapped
// alpha/numeric fallback used for model names such as codex52 vs gpt-5.2-codex.
func MatchString(query, text string) Match {
	queryLower := strings.ToLower(query)
	textLower := strings.ToLower(text)

	primary := matchNormalized(queryLower, textLower)
	if primary.Matches {
		return primary
	}

	swapped := swappedAlphaNumeric(queryLower)
	if swapped == "" {
		return primary
	}

	swappedMatch := matchNormalized(swapped, textLower)
	if !swappedMatch.Matches {
		return primary
	}

	swappedMatch.Score += 5

	return swappedMatch
}

// Filter returns the items whose text matches all space- or slash-separated tokens in
// query, sorted by total fuzzy score. Empty or whitespace-only queries return a
// shallow copy in the original order.
func Filter[T any](items []T, query string, text func(T) string) []T {
	if strings.TrimSpace(query) == "" {
		out := make([]T, len(items))
		copy(out, items)

		return out
	}

	tokens := strings.FieldsFunc(query, func(r rune) bool { return r == '/' || unicode.IsSpace(r) })
	if len(tokens) == 0 {
		out := make([]T, len(items))
		copy(out, items)

		return out
	}

	type result struct {
		item  T
		score float64
		idx   int
	}

	results := make([]result, 0, len(items))
	for i, item := range items {
		itemText := text(item)
		totalScore := 0.0
		allMatch := true

		for _, token := range tokens {
			match := MatchString(token, itemText)
			if !match.Matches {
				allMatch = false
				break
			}
			totalScore += match.Score
		}

		if allMatch {
			results = append(results, result{item: item, score: totalScore, idx: i})
		}
	}

	sort.SliceStable(results, func(i, j int) bool {
		if results[i].score == results[j].score {
			return results[i].idx < results[j].idx
		}

		return results[i].score < results[j].score
	})

	out := make([]T, len(results))
	for i, result := range results {
		out[i] = result.item
	}

	return out
}

func matchNormalized(queryLower, textLower string) Match {
	if queryLower == "" {
		return Match{Matches: true, Score: 0}
	}

	query := []rune(queryLower)
	text := []rune(textLower)
	if len(query) > len(text) {
		return Match{Matches: false, Score: 0}
	}

	queryIndex := 0
	score := 0.0
	lastMatchIndex := -1
	consecutiveMatches := 0

	for i := 0; i < len(text) && queryIndex < len(query); i++ {
		if text[i] != query[queryIndex] {
			continue
		}

		wordBoundary := i == 0 || isBoundary(text[i-1])
		if lastMatchIndex == i-1 {
			consecutiveMatches++
			score -= float64(consecutiveMatches * 5)
		} else {
			consecutiveMatches = 0
			if lastMatchIndex >= 0 {
				score += float64((i - lastMatchIndex - 1) * 2)
			}
		}

		if wordBoundary {
			score -= 10
		}

		score += float64(i) * 0.1
		lastMatchIndex = i
		queryIndex++
	}

	if queryIndex < len(query) {
		return Match{Matches: false, Score: 0}
	}

	if queryLower == textLower {
		score -= 100
	}

	return Match{Matches: true, Score: score}
}

func isBoundary(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '-', '_', '.', '/', ':':
		return true
	default:
		return false
	}
}

func swappedAlphaNumeric(query string) string {
	if !utf8.ValidString(query) {
		return ""
	}

	if matches := alphaNumericRe.FindStringSubmatch(query); len(matches) == 3 {
		return matches[2] + matches[1]
	}

	if matches := numericAlphaRe.FindStringSubmatch(query); len(matches) == 3 {
		return matches[2] + matches[1]
	}

	return ""
}
