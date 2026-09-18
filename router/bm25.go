package router

import (
	"math"
	"regexp"
	"sort"
	"strings"
)

var tokenPattern = regexp.MustCompile(`[\p{L}\p{N}][\p{L}\p{N}_+.-]*`)

var stopWords = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "for": {}, "i": {}, "is": {}, "me": {}, "my": {},
	"of": {}, "please": {}, "the": {}, "to": {}, "tool": {}, "use": {}, "want": {},
}

func tokenize(text string) []string {
	words := tokenPattern.FindAllString(strings.ToLower(text), -1)
	result := words[:0]
	for _, word := range words {
		if _, ignored := stopWords[word]; !ignored {
			stemmed := stem(word)
			if stemmed != "" {
				result = append(result, stemmed)
			}
		}
	}
	return result
}

// stem implements a lightweight English suffix-stripping stemmer.
// It normalizes common verb and noun inflections (e.g. -ing, -ed, -es, -s)
// to their base forms, improving search recall with zero external dependencies.
func stem(word string) string {
	if len(word) <= 3 {
		return word
	}
	if strings.HasSuffix(word, "sses") {
		return word[:len(word)-2]
	}
	if strings.HasSuffix(word, "ies") {
		return word[:len(word)-3] + "i"
	}
	if strings.HasSuffix(word, "ing") {
		return strings.TrimSuffix(word, "ing")
	}
	if strings.HasSuffix(word, "ed") {
		return strings.TrimSuffix(word, "ed")
	}
	if strings.HasSuffix(word, "es") {
		return strings.TrimSuffix(word, "es")
	}
	if strings.HasSuffix(word, "s") && !strings.HasSuffix(word, "ss") {
		return word[:len(word)-1]
	}
	return word
}

type posting struct {
	docIndex int
	tf       int
}

type lexicalIndex struct {
	documents     [][]string
	docLengths    []int
	invertedIndex map[string][]posting
	idf           map[string]float64
	avgLength     float64
}

func newLexicalIndex(texts []string) *lexicalIndex {
	index := &lexicalIndex{
		documents:     make([][]string, 0, len(texts)),
		docLengths:    make([]int, len(texts)),
		invertedIndex: make(map[string][]posting),
		idf:           make(map[string]float64),
	}
	documentFrequency := make(map[string]int)
	for docIndex, text := range texts {
		tokens := tokenize(text)
		index.documents = append(index.documents, tokens)
		index.docLengths[docIndex] = len(tokens)
		index.avgLength += float64(len(tokens))
		tfMap := make(map[string]int)
		for _, token := range tokens {
			tfMap[token]++
		}
		for token, count := range tfMap {
			documentFrequency[token]++
			index.invertedIndex[token] = append(index.invertedIndex[token], posting{
				docIndex: docIndex,
				tf:       count,
			})
		}
	}
	if len(texts) > 0 {
		index.avgLength /= float64(len(texts))
	}
	for token, frequency := range documentFrequency {
		index.idf[token] = math.Log(1 + (float64(len(texts)-frequency)+0.5)/(float64(frequency)+0.5))
	}
	return index
}

func (i *lexicalIndex) scores(query string) []float64 {
	scores := make([]float64, len(i.docLengths))
	if len(i.docLengths) == 0 || i.avgLength == 0 {
		return scores
	}
	queryTokens := tokenize(query)
	const k1, b = 1.2, 0.75
	for _, token := range queryTokens {
		idf, exists := i.idf[token]
		if !exists || idf <= 0 {
			continue
		}
		postings := i.invertedIndex[token]
		for _, p := range postings {
			count := float64(p.tf)
			docLen := float64(i.docLengths[p.docIndex])
			denominator := count + k1*(1-b+b*docLen/i.avgLength)
			scores[p.docIndex] += idf * count * (k1 + 1) / denominator
		}
	}
	return scores
}

func normalizeScores(scores []float64) []float64 {
	result := append([]float64(nil), scores...)
	maximum := 0.0
	for _, score := range result {
		if score > maximum {
			maximum = score
		}
	}
	if maximum == 0 {
		return result
	}
	for index := range result {
		result[index] /= maximum
	}
	return result
}

func rankedIndices(scores []float64) []int {
	indices := make([]int, len(scores))
	for index := range indices {
		indices[index] = index
	}
	sort.SliceStable(indices, func(i, j int) bool {
		if scores[indices[i]] == scores[indices[j]] {
			return indices[i] < indices[j]
		}
		return scores[indices[i]] > scores[indices[j]]
	})
	return indices
}
