package document

import (
	"context"
	"strings"
	"time"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

const MaxHighlightBytes = 128 << 10

// Highlight is best effort. Unknown languages and expensive inputs stay readable.
// No global style registry is mutated by concurrent document renderers.
func highlight(ctx context.Context, source, path, language string) (result string) {
	result = source
	defer func() {
		if recover() != nil {
			result = source
		}
	}()
	if len(source) > MaxHighlightBytes || ctx.Err() != nil {
		return
	}
	var lexer chroma.Lexer
	if language != "" {
		lexer = lexers.Get(language)
	} else {
		lexer = lexers.Match(path)
	}
	if lexer == nil {
		return
	}
	it, err := lexer.Tokenise(nil, source)
	if err != nil {
		return
	}
	deadline := time.Now().Add(200 * time.Millisecond)
	var out strings.Builder
	for token := it(); token != chroma.EOF; token = it() {
		if ctx.Err() != nil || time.Now().After(deadline) {
			return
		}
		// End styles before each newline so scrolling never depends on an offscreen row.
		for i, part := range strings.Split(token.Value, "\n") {
			if i > 0 {
				out.WriteByte('\n')
			}
			if part == "" {
				continue
			}
			if err := formatters.TTY16m.Format(&out, styles.Get("dracula"), chroma.Literator(chroma.Token{Type: token.Type, Value: part})); err != nil {
				return
			}
		}
	}
	return safeANSI(out.String())
}
