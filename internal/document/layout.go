package document

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	glam "github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

const MaxLines = 12000

var sgr = regexp.MustCompile(`\x1b\[[0-9;:]*m`)

func safeANSI(s string) string {
	var out strings.Builder
	clean := func(part string) {
		out.WriteString(strings.Map(func(r rune) rune {
			if r != '\n' && r != '\t' && (unicode.IsControl(r) || unicode.In(r, unicode.Cf)) {
				return '�'
			}
			return r
		}, part))
	}
	at := 0
	for _, loc := range sgr.FindAllStringIndex(s, -1) {
		clean(s[at:loc[0]])
		out.WriteString(s[loc[0]:loc[1]])
		at = loc[1]
	}
	clean(s[at:])
	return out.String()
}

type Line struct {
	Text       string
	SourceLine int
}
type Asset struct {
	Kind, Ref, Alt, Source string
	Line, SourceLine       int
	ReplaceFrom            int // one-based first row replaced by a diagram/loading slot; zero appends
}
type Layout struct {
	Lines  []Line
	Assets []Asset
}

func (l *Layout) add(s string, sourceLine, width int) {
	for _, line := range strings.Split(s, "\n") {
		if len(l.Lines) >= MaxLines {
			return
		}
		l.Lines = append(l.Lines, Line{ansi.Truncate(line, width, ""), sourceLine})
	}
}

func Render(ctx context.Context, d *Document, width int, raw bool) (Layout, error) {
	width = max(4, width)
	var layout Layout
	source := []byte(d.Source)
	if raw || d.Kind != "MD" {
		styled := d.Source
		if !raw {
			styled = highlight(ctx, d.Source, d.Path, "")
		}
		for i, line := range strings.Split(styled, "\n") {
			if len(layout.Lines) >= MaxLines {
				break
			}
			if err := ctx.Err(); err != nil {
				return layout, err
			}
			line = strings.ReplaceAll(line, "\t", "    ")
			layout.add(ansi.Hardwrap(line, width, true), i, width)
		}
		if d.Kind == "MMD" && !raw {
			layout.Assets = append(layout.Assets, Asset{Kind: "mermaid", Source: d.Source, Line: len(layout.Lines), ReplaceFrom: 1})
		}
	} else {
		md := goldmark.New(goldmark.WithExtensions(extension.GFM, extension.DefinitionList))
		root := md.Parser().Parse(text.NewReader(source))
		for node := root.FirstChild(); node != nil && len(layout.Lines) < MaxLines; {
			if err := ctx.Err(); err != nil {
				return layout, err
			}
			next := node.NextSibling()
			sourceLine := nodeLine(node, source)
			var assets []Asset
			_ = ast.Walk(node, func(n ast.Node, enter bool) (ast.WalkStatus, error) {
				if !enter {
					return ast.WalkContinue, nil
				}
				if im, ok := n.(*ast.Image); ok {
					assets = append(assets, Asset{Kind: "image", Ref: string(im.Destination), Alt: string(im.Text(source)), SourceLine: sourceLine})
				}
				if code, ok := n.(*ast.FencedCodeBlock); ok && strings.EqualFold(string(code.Language(source)), "mermaid") {
					a := Asset{Kind: "mermaid", Source: string(code.Lines().Value(source)), SourceLine: sourceLine}
					if n == node {
						a.ReplaceFrom = len(layout.Lines) + 1
					}
					assets = append(assets, a)
				}
				return ast.WalkContinue, nil
			})
			// Render complete top-level blocks so list/table context stays intact. The
			// same parsed AST resolves reference-style images and retains asset order.
			one := ast.NewDocument()
			one.AppendChild(one, node)
			style := styles.DarkStyleConfig
			fg, accent := "#dae2ef", "#58d6be"
			style.Document.Color = &fg
			style.Heading.Color = &accent
			style.H1.Color = &accent
			style.H1.BackgroundColor = nil
			style.H1.Prefix = ""
			style.H1.Suffix = ""
			style.CodeBlock.Chroma = nil
			style.CodeBlock.Theme = ""
			if len(d.Source) <= MaxHighlightBytes {
				style.CodeBlock.Theme = "dracula"
			}
			zero := uint(0)
			style.Document.Margin = &zero
			ar := glam.NewRenderer(glam.Options{WordWrap: width, ColorProfile: termenv.TrueColor, Styles: style, ChromaFormatter: "terminal16m"})
			r := renderer.NewRenderer(renderer.WithNodeRenderers(util.Prioritized(ar, 1000)))
			var out bytes.Buffer
			if err := r.Render(&out, source, one); err != nil {
				return layout, fmt.Errorf("render Markdown: %w", err)
			}
			renderedRows := strings.Split(safeANSI(out.String()), "\n")
			for len(renderedRows) > 0 && strings.TrimSpace(ansi.Strip(renderedRows[0])) == "" {
				renderedRows = renderedRows[1:]
			}
			for len(renderedRows) > 0 && strings.TrimSpace(ansi.Strip(renderedRows[len(renderedRows)-1])) == "" {
				renderedRows = renderedRows[:len(renderedRows)-1]
			}
			rendered := strings.Join(renderedRows, "\n")
			layout.add(rendered, sourceLine, width)
			for _, a := range assets {
				a.Line = len(layout.Lines)
				layout.Assets = append(layout.Assets, a)
			}
			layout.add("", sourceLine, width)
			node = next
		}
	}
	if len(layout.Lines) >= MaxLines {
		layout.Lines = append(layout.Lines, Line{Text: "… Preview limited to 12000 rows", SourceLine: layout.Lines[len(layout.Lines)-1].SourceLine})
	}
	if d.Truncated {
		layout.add("… Preview truncated at 2 MiB", len(strings.Split(d.Source, "\n"))-1, width)
	}
	if len(layout.Lines) == 0 {
		layout.add("(empty file)", 0, width)
	}
	return layout, nil
}

func nodeLine(n ast.Node, source []byte) int {
	start := len(source)
	_ = ast.Walk(n, func(child ast.Node, enter bool) (ast.WalkStatus, error) {
		if enter && child.Type() != ast.TypeInline && child.Lines() != nil && child.Lines().Len() > 0 {
			start = min(start, child.Lines().At(0).Start)
		}
		if t, ok := child.(*ast.Text); ok && enter {
			start = min(start, t.Segment.Start)
		}
		return ast.WalkContinue, nil
	})
	if start == len(source) {
		return 0
	}
	return bytes.Count(source[:start], []byte{'\n'})
}
