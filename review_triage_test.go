package mecp

import "testing"

// These cases are real: each pair is a line from an instruction document and
// the statement an agent wrote from it during dogfooding.
func TestQuoteGrounding(t *testing.T) {
	tests := []struct {
		name      string
		statement string
		quote     string
		grounded  bool
	}{
		{
			name:      "summarizing a table row keeps its meaning",
			statement: "Never run `pkill` or `killall`.",
			quote:     "| `pkill`, `killall` | BANNED. No exceptions — a name pattern cannot tell your process from someone else's |",
			grounded:  true,
		},
		{
			name:      "a normalized bullet stays grounded",
			statement: "Use small single-value command output directly rather than storing it to a file.",
			quote:     "Small single-value output (`git rev-parse HEAD`, `wc -l` counts) → use directly, no file",
			grounded:  true,
		},
		{
			name:      "an arrow becomes a sentence",
			statement: "When a command offers no directory flag, run `cd <dir>` as its own Bash call, then the command separately.",
			quote:     "No flag/absolute-path option → `cd <dir>` as its own Bash call, then the command as a separate call.",
			grounded:  true,
		},
		{
			name:      "a statement about something else entirely is not grounded",
			statement: "Deployment happens every Friday afternoon without exception.",
			quote:     "Do not use named return values.",
			grounded:  false,
		},
		{
			name:      "a plausible-sounding invention is not grounded",
			statement: "Always run the linter before committing any change.",
			quote:     "NEVER use `/tmp/`. Use `$PROJECT_DIR/.tmp/` — create if missing.",
			grounded:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := quoteGrounding(tt.statement, tt.quote)
			if tt.grounded && got < minQuoteGrounding {
				t.Errorf("grounding %.2f is below the %.2f bar, so a good record would be held", got, minQuoteGrounding)
			}
			if !tt.grounded && got >= minQuoteGrounding {
				t.Errorf("grounding %.2f clears the %.2f bar, so an invented record would be activated", got, minQuoteGrounding)
			}
		})
	}
}
