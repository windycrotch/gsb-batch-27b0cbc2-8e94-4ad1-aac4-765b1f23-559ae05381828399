package filter

import (
	"fmt"
	"strings"

	"github.com/spiretechnology/spireav/filter/expr"
)

// Filter is a node that applies a filter to its inputs, producing one or more outputs.
type Filter interface {
	With(key string, value expr.Expr) Filter
	String() string
	Outputs() int
}

// Option defines an output option middleware that populates some option string
// into the slice of options for the output.
type Option func() []OptionValue

// New creates a new transform with the given name and some options.
func New(name string, outputs int, opts ...Option) Filter {
	return &filterWithOpts{
		name:    name,
		outputs: outputs,
		opts:    WithMulti(opts...)(),
	}
}

// WithOption creates a new option middleware with a key and value.
func WithOption(key string, value expr.Expr) Option {
	return func() []OptionValue {
		return []OptionValue{
			{
				Key: key,
				Val: value,
			},
		}
	}
}

// WithMulti creates a new option middleware with multiple options.
func WithMulti(opts ...Option) Option {
	return func() []OptionValue {
		var options []OptionValue
		for _, opt := range opts {
			options = append(options, opt()...)
		}
		return options
	}
}

// OptionValue defines a single option for a transform node. This includes only a single
// key-value pair.
type OptionValue struct {
	Key string
	Val expr.Expr
}

type filterWithOpts struct {
	name    string
	outputs int
	opts    []OptionValue
}

func (t *filterWithOpts) With(key string, value expr.Expr) Filter {
	// Copy the options into a fresh slice so the parent filter's options are never
	// mutated through a shared backing array.
	opts := make([]OptionValue, len(t.opts)+1)
	copy(opts, t.opts)
	opts[len(t.opts)] = OptionValue{Key: key, Val: value}
	return &filterWithOpts{
		name:    t.name,
		outputs: t.outputs,
		opts:    opts,
	}
}

func (t *filterWithOpts) String() string {
	// Build the options deterministically in insertion order. If the same key is set
	// multiple times, the last value wins but the key keeps its original position.
	var keyvals []string
	positions := make(map[string]int, len(t.opts))
	for _, optValue := range t.opts {
		pair := fmt.Sprintf("%s=%s", optValue.Key, formatTransformOptionValue(optValue.Val.String()))
		if pos, ok := positions[optValue.Key]; ok {
			keyvals[pos] = pair
		} else {
			positions[optValue.Key] = len(keyvals)
			keyvals = append(keyvals, pair)
		}
	}
	return formatTransform(t.name, keyvals)
}

func (t *filterWithOpts) Outputs() int {
	return t.outputs
}

// formatTransform formats a filter transform so that it conforms to the FFMpeg format
func formatTransform(filterName string, keyvals []string) string {
	if len(keyvals) == 0 {
		return filterName
	}
	return fmt.Sprintf("%s=%s", filterName, strings.Join(keyvals, ":"))
}

// formatTransformOptionValue formats a single value to ensure it's properly escaped and quote-wrapped
func formatTransformOptionValue(value string) string {
	if strings.ContainsAny(value, ":,()") && !strings.HasPrefix(value, "'") {
		return fmt.Sprintf("'%s'", value)
	}
	return value
}

type rawFilter struct {
	str     string
	outputs int
}

func Raw(str string, outputs int) Filter {
	return &rawFilter{
		str:     str,
		outputs: outputs,
	}
}

func (f *rawFilter) With(key string, value expr.Expr) Filter {
	return f
}

func (f *rawFilter) String() string {
	return f.str
}

func (f *rawFilter) Outputs() int {
	return f.outputs
}
