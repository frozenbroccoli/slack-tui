package source

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/kurenn/slack-tui/internal/data"
	"golang.org/x/net/html"
)

var canvasWhitespace = regexp.MustCompile(`\s+`)

// Export conversion preserves common document structure. Unknown rich nodes
// remain readable but disable whole-document replacement to avoid dropping them.
func canvasHTMLToMarkdown(raw string) (string, []string, error) {
	if !strings.Contains(raw, "<") {
		return "", nil, fmt.Errorf("Slack returned non-HTML Canvas content")
	}
	root, err := html.Parse(strings.NewReader(raw))
	if err != nil {
		return "", nil, fmt.Errorf("parse Canvas export: %w", err)
	}
	warnings := map[string]bool{}
	var render func(*html.Node) string
	var children func(*html.Node) string
	children = func(n *html.Node) string {
		var b strings.Builder
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			b.WriteString(render(c))
		}
		return b.String()
	}
	render = func(n *html.Node) string {
		if n.Type == html.TextNode {
			return escapeCanvasText(canvasWhitespace.ReplaceAllString(n.Data, " "))
		}
		if n.Type != html.ElementNode {
			return children(n)
		}
		tag := n.Data
		switch tag {
		case "head", "script", "style":
			return ""
		case "form":
			warnings["form"] = true
			return ""
		}
		for _, a := range n.Attr {
			if (a.Key == "class" && (strings.Contains(a.Val, "unfurl") || strings.Contains(a.Val, "mention") || strings.Contains(a.Val, "callout") || strings.Contains(a.Val, "flexbox"))) || (strings.HasPrefix(a.Key, "data-") && a.Key != "data-section-id") {
				warnings["Slack rich content"] = true
			}
			if a.Key == "style" && tag != "body" {
				warnings["custom styling"] = true
			}
		}
		switch tag {
		case "html", "body", "div", "section", "article", "span":
			return children(n)
		case "p":
			return "\n\n" + strings.TrimSpace(children(n)) + "\n\n"
		case "h1", "h2", "h3":
			return "\n\n" + strings.Repeat("#", int(tag[1]-'0')) + " " + strings.TrimSpace(children(n)) + "\n\n"
		case "br":
			return "  \n"
		case "hr":
			return "\n\n---\n\n"
		case "strong", "b":
			return "**" + children(n) + "**"
		case "em", "i":
			return "*" + children(n) + "*"
		case "s", "del", "strike":
			return "~~" + children(n) + "~~"
		case "code":
			text := canvasNodeText(n)
			fence := canvasFence(text, 1)
			return fence + " " + text + " " + fence
		case "pre":
			text := strings.TrimSuffix(canvasNodeText(n), "\n")
			fence := canvasFence(text, 3)
			return "\n\n" + fence + "\n" + text + "\n" + fence + "\n\n"
		case "a":
			href := canvasAttr(n, "href")
			label := children(n)
			if href == "" {
				return label
			}
			if strings.HasPrefix(href, "javascript:") {
				warnings["unsupported link"] = true
				return label
			}
			return "[" + label + "](<" + strings.ReplaceAll(href, ">", "%3E") + ">)"
		case "img":
			src := canvasAttr(n, "src")
			warnings["image or embedded object"] = true
			return "![" + canvasAttr(n, "alt") + "](<" + src + ">)"
		case "ul", "ol":
			return "\n\n" + strings.TrimSpace(children(n)) + "\n\n"
		case "li":
			content := strings.TrimSpace(children(n))
			lines := strings.Split(content, "\n")
			marker := "- "
			if n.Parent != nil && n.Parent.Data == "ol" {
				start := 1
				if v, e := strconv.Atoi(canvasAttr(n.Parent, "start")); e == nil {
					start = v
				}
				for c := n.PrevSibling; c != nil; c = c.PrevSibling {
					if c.Type == html.ElementNode && c.Data == "li" {
						start++
					}
				}
				marker = fmt.Sprintf("%d. ", start)
			}
			for i := 1; i < len(lines); i++ {
				lines[i] = strings.Repeat(" ", len(marker)) + lines[i]
			}
			return marker + strings.Join(lines, "\n") + "\n"
		case "input":
			if canvasAttr(n, "type") == "checkbox" {
				for _, a := range n.Attr {
					if a.Key == "checked" {
						return "[x] "
					}
				}
				return "[ ] "
			}
			warnings["input"] = true
			return ""
		case "blockquote":
			content := strings.TrimSpace(children(n))
			return "\n\n> " + strings.ReplaceAll(content, "\n", "\n> ") + "\n\n"
		case "table":
			var rows [][]string
			var walk func(*html.Node)
			walk = func(node *html.Node) {
				if node.Type == html.ElementNode && node.Data == "tr" {
					var cells []string
					for c := node.FirstChild; c != nil; c = c.NextSibling {
						if c.Data == "td" || c.Data == "th" {
							if canvasAttr(c, "colspan") != "" || canvasAttr(c, "rowspan") != "" {
								warnings["merged table cells"] = true
							}
							cell := strings.TrimSpace(children(c))
							cell = strings.ReplaceAll(cell, "\n", "<br>")
							cell = strings.ReplaceAll(cell, "|", "\\|")
							cells = append(cells, cell)
						}
					}
					rows = append(rows, cells)
					return
				}
				for c := node.FirstChild; c != nil; c = c.NextSibling {
					walk(c)
				}
			}
			walk(n)
			if len(rows) == 0 {
				return ""
			}
			cols := len(rows[0])
			var lines []string
			for i, row := range rows {
				if len(row) != cols {
					warnings["irregular table"] = true
				}
				lines = append(lines, "| "+strings.Join(row, " | ")+" |")
				if i == 0 {
					separators := make([]string, cols)
					for j := range separators {
						separators[j] = "---"
					}
					lines = append(lines, "| "+strings.Join(separators, " | ")+" |")
				}
			}
			return "\n\n" + strings.Join(lines, "\n") + "\n\n"
		default:
			warnings[tag] = true
			content := children(n)
			if strings.TrimSpace(content) == "" {
				content = "[" + tag + "]"
			}
			return content
		}
	}
	markdown := strings.TrimSpace(render(root))
	for strings.Contains(markdown, "\n\n\n") {
		markdown = strings.ReplaceAll(markdown, "\n\n\n", "\n\n")
	}
	var out []string
	for warning := range warnings {
		out = append(out, warning)
	}
	sort.Strings(out)
	if warnings["form"] {
		return "", nil, fmt.Errorf("Slack returned a form instead of a Canvas export; reauthorize files:read")
	}
	return markdown, out, nil
}

func canvasAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}
func canvasNodeText(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		b.WriteString(canvasNodeText(c))
	}
	return b.String()
}
func canvasFence(text string, minimum int) string {
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", max(minimum, longest+1))
}
func escapeCanvasText(text string) string {
	return strings.NewReplacer("\\", "\\\\", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "`", "\\`", "#", "\\#").Replace(text)
}

func canvasHTMLSections(raw string) []data.CanvasSection {
	root, err := html.Parse(strings.NewReader(raw))
	if err != nil {
		return nil
	}
	var sections []data.CanvasSection
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		id := canvasAttr(n, "data-section-id")
		if id == "" {
			id = canvasAttr(n, "id")
		}
		if n.Type == html.ElementNode && id != "" {
			switch n.Data {
			case "p", "h1", "h2", "h3", "li", "ul", "ol", "blockquote", "pre", "table", "hr":
				var b strings.Builder
				if html.Render(&b, n) == nil {
					markdown, warnings, err := canvasHTMLToMarkdown(b.String())
					if err == nil {
						sections = append(sections, data.CanvasSection{ID: id, Markdown: markdown, Editable: len(warnings) == 0})
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return sections
}
