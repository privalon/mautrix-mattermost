// Copyright 2024-2026 Remi Philippe
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Package mattermostfmt converts Mattermost markdown to Matrix HTML.
package mattermostfmt

import (
	"html"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"maunium.net/go/mautrix/event"
)

// ParsedMessage holds the result of converting Mattermost markdown to Matrix format.
type ParsedMessage struct {
	Body          string
	Format        event.Format
	FormattedBody string
	RelatesTo     *event.RelatesTo
}

var (
	boldRe       = regexp.MustCompile(`\*\*(.+?)\*\*`)
	urlRe        = regexp.MustCompile(`https?://[^\s<>"]+`)
	strikeRe     = regexp.MustCompile(`~~(.+?)~~`)
	codeRe       = regexp.MustCompile("`([^`]+)`")
	codeBlockRe  = regexp.MustCompile("(?s)```(\\w+)?\\n?(.*?)```")
	linkRe       = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	headingRe    = regexp.MustCompile(`(?m)^(#{1,6})\s+(.+)$`)
	ulRe         = regexp.MustCompile(`(?m)^[-*]\s+(.+)$`)
	olRe         = regexp.MustCompile(`(?m)^(\d+)\.\s+(.+)$`)
	contRe       = regexp.MustCompile(`^(?:\s{2,}|\t)\S`)
	blockGapRe   = regexp.MustCompile(`\n*(<(?:ul|ol|blockquote|h[1-6])[ >])`)
	blockEndRe   = regexp.MustCompile(`(</(?:ul|ol|blockquote|h[1-6])>)\n*`)
	blockquoteRe = regexp.MustCompile(`(?m)^>\s+(.+)$`)
)

// codeBlock holds extracted code block data.
type codeBlock struct {
	lang    string
	content string
}

func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// italicizeSegment wraps `_text_` in <em>, following CommonMark: an opening
// underscore must not follow a word character (or `*`) and be followed by a
// non-space; the closing one must follow a non-space and not be followed by a
// word character (or `*`). Underscores inside identifiers such as
// `skipr_vpn_desktop` are therefore left alone. Unlike a regex that consumes
// its boundary characters, this never swallows the characters around the
// underscores.
func italicizeSegment(s string) string {
	rs := []rune(s)
	var out strings.Builder
	i := 0
	for i < len(rs) {
		if rs[i] != '_' {
			out.WriteRune(rs[i])
			i++
			continue
		}
		openOK := (i == 0 || (!isWordRune(rs[i-1]) && rs[i-1] != '*')) &&
			i+1 < len(rs) && !unicode.IsSpace(rs[i+1]) && rs[i+1] != '_'
		closeAt := -1
		if openOK {
			for j := i + 2; j < len(rs); j++ {
				if rs[j] != '_' || unicode.IsSpace(rs[j-1]) || rs[j-1] == '_' {
					continue
				}
				if j+1 == len(rs) || (!isWordRune(rs[j+1]) && rs[j+1] != '*') {
					closeAt = j
					break
				}
			}
		}
		if closeAt < 0 {
			out.WriteRune('_')
			i++
			continue
		}
		out.WriteString("<em>")
		out.WriteString(string(rs[i+1 : closeAt]))
		out.WriteString("</em>")
		i = closeAt + 1
	}
	return out.String()
}

// italicize applies italicizeSegment to everything except URLs, so a link
// like https://host/_a_/b is never altered.
func italicize(s string) string {
	locs := urlRe.FindAllStringIndex(s, -1)
	if len(locs) == 0 {
		return italicizeSegment(s)
	}
	var out strings.Builder
	last := 0
	for _, l := range locs {
		out.WriteString(italicizeSegment(s[last:l[0]]))
		out.WriteString(s[l[0]:l[1]])
		last = l[1]
	}
	out.WriteString(italicizeSegment(s[last:]))
	return out.String()
}

func hasItalic(s string) bool { return italicize(s) != s }

// Parse converts a Mattermost markdown message to Matrix event content.
func Parse(text string) *ParsedMessage {
	if text == "" {
		return &ParsedMessage{}
	}

	hasFormatting := boldRe.MatchString(text) ||
		hasItalic(text) ||
		strikeRe.MatchString(text) ||
		codeRe.MatchString(text) ||
		codeBlockRe.MatchString(text) ||
		linkRe.MatchString(text) ||
		headingRe.MatchString(text) ||
		blockquoteRe.MatchString(text) ||
		ulRe.MatchString(text) ||
		olRe.MatchString(text)

	if !hasFormatting {
		return &ParsedMessage{Body: text}
	}

	// Step 1: Extract code blocks into placeholders.
	var codeBlocks []codeBlock
	processed := codeBlockRe.ReplaceAllStringFunc(text, func(match string) string {
		parts := codeBlockRe.FindStringSubmatch(match)
		lang := ""
		content := ""
		if len(parts) >= 3 {
			lang = parts[1]
			content = parts[2]
		} else if len(parts) >= 2 {
			content = parts[1]
		}
		idx := len(codeBlocks)
		codeBlocks = append(codeBlocks, codeBlock{lang: lang, content: content})
		return "\x00CODEBLOCK" + strconv.Itoa(idx) + "\x00"
	})

	// Step 2: Process line-by-line for structural elements on raw text.
	lines := strings.Split(processed, "\n")
	var result []string
	var listType string // "ul", "ol", or ""
	var listItems []string
	olStart := ""

	flushList := func() {
		if len(listItems) == 0 {
			return
		}
		open := "<" + listType + ">"
		if listType == "ol" && olStart != "" && olStart != "1" {
			open = `<ol start="` + olStart + `">`
		}
		result = append(result, open+strings.Join(listItems, "")+"</"+listType+">")
		listItems = nil
		listType = ""
		olStart = ""
	}

	for i, line := range lines {
		// Indented continuation line inside a list item (or a blank line
		// between items of the same list) stays within the list.
		if len(listItems) > 0 {
			if contRe.MatchString(line) && !ulRe.MatchString(line) && !olRe.MatchString(line) {
				last := len(listItems) - 1
				listItems[last] = strings.TrimSuffix(listItems[last], "</li>") + "<br/>" + html.EscapeString(strings.TrimSpace(line)) + "</li>"
				continue
			}
			if strings.TrimSpace(line) == "" && i+1 < len(lines) {
				next := lines[i+1]
				if (listType == "ul" && ulRe.MatchString(next)) || (listType == "ol" && olRe.MatchString(next)) {
					continue
				}
			}
		}

		// Check blockquote.
		if m := blockquoteRe.FindStringSubmatch(line); len(m) >= 2 {
			flushList()
			result = append(result, "<blockquote>"+html.EscapeString(m[1])+"</blockquote>")
			continue
		}

		// Check heading.
		if m := headingRe.FindStringSubmatch(line); len(m) >= 3 {
			flushList()
			level := min(len(m[1]), 6)
			lvl := strconv.Itoa(level)
			result = append(result, "<h"+lvl+">"+html.EscapeString(m[2])+"</h"+lvl+">")
			continue
		}

		// Check unordered list.
		if m := ulRe.FindStringSubmatch(line); len(m) >= 2 {
			if listType != "ul" {
				flushList()
				listType = "ul"
			}
			listItems = append(listItems, "<li>"+html.EscapeString(m[1])+"</li>")
			continue
		}

		// Check ordered list.
		if m := olRe.FindStringSubmatch(line); len(m) >= 2 {
			if listType != "ol" {
				flushList()
				listType = "ol"
				olStart = m[1]
			}
			listItems = append(listItems, "<li>"+html.EscapeString(m[2])+"</li>")
			continue
		}

		// Regular line.
		flushList()
		result = append(result, html.EscapeString(line))
	}
	flushList()

	formatted := strings.Join(result, "\n")

	// Block elements carry their own spacing; drop adjacent newlines so they
	// don't turn into stray <br/> tags.
	formatted = blockGapRe.ReplaceAllString(formatted, "$1")
	formatted = blockEndRe.ReplaceAllString(formatted, "$1")

	// Step 3: Inline formatting.
	formatted = codeRe.ReplaceAllString(formatted, "<code>$1</code>")
	formatted = boldRe.ReplaceAllString(formatted, "<strong>$1</strong>")
	formatted = italicize(formatted)
	formatted = strikeRe.ReplaceAllString(formatted, "<del>$1</del>")

	// Links — only allow safe URL schemes.
	formatted = linkRe.ReplaceAllStringFunc(formatted, func(match string) string {
		parts := linkRe.FindStringSubmatch(match)
		if len(parts) < 3 {
			return match
		}
		text, href := parts[1], parts[2]
		lower := strings.ToLower(strings.TrimSpace(href))
		if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "mailto:") {
			return `<a href="` + href + `">` + text + `</a>`
		}
		// Unsafe scheme (javascript:, data:, etc.) — render as plain text.
		return text
	})

	// Step 4: Restore code blocks with language hints.
	for i, cb := range codeBlocks {
		placeholder := "\x00CODEBLOCK" + strconv.Itoa(i) + "\x00"
		escapedContent := html.EscapeString(cb.content)
		var replacement string
		if cb.lang != "" {
			replacement = `<pre><code class="language-` + html.EscapeString(cb.lang) + `">` + escapedContent + `</code></pre>`
		} else {
			replacement = `<pre><code>` + escapedContent + `</code></pre>`
		}
		formatted = strings.Replace(formatted, placeholder, replacement, 1)
	}

	// Step 5: Paragraphs (double newlines).
	formatted = strings.ReplaceAll(formatted, "\n\n", "</p><p>")

	// Step 6: Line breaks (remaining single newlines).
	formatted = strings.ReplaceAll(formatted, "\n", "<br/>")

	// Wrap in paragraph tags if we have paragraph breaks.
	if strings.Contains(formatted, "</p><p>") {
		formatted = "<p>" + formatted + "</p>"
	}

	return &ParsedMessage{
		Body:          text,
		Format:        event.FormatHTML,
		FormattedBody: formatted,
	}
}
