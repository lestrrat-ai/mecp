package source

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/lestrrat-ai/mecp"
)

// exportEnvelope wraps each JSONL line so that a file can carry records,
// proposals, and a format version without needing a separate manifest.
type exportEnvelope struct {
	Format  string         `json:"format"`
	Version int            `json:"version"`
	Type    string         `json:"type"`
	Record  *mecp.Record   `json:"record,omitempty"`
	Prop    *mecp.Proposal `json:"proposal,omitempty"`
}

const (
	exportFormat  = "mecp-export"
	exportVersion = 1
)

// ExportJSONL writes every record, and optionally every proposal, as one JSON
// object per line, ordered by ID so that two exports of the same data are
// byte-identical.
//
// The format deliberately avoids the FTS and index representation: a portable
// export is what makes the data the user's rather than the tool's.
func ExportJSONL(ctx context.Context, store mecp.Store, w io.Writer, includeProposals bool) (int, error) {
	recs, err := store.QueryRecords(ctx, mecp.RecordQuery{})
	if err != nil {
		return 0, fmt.Errorf(`failed to read records: %w`, err)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].ID < recs[j].ID })

	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)

	var written int
	for _, rec := range recs {
		if err := enc.Encode(exportEnvelope{
			Format: exportFormat, Version: exportVersion, Type: "record", Record: rec,
		}); err != nil {
			return written, fmt.Errorf(`failed to write record %s: %w`, rec.ID, err)
		}
		written++
	}

	if includeProposals {
		props, err := store.QueryProposals(ctx, mecp.ProposalQuery{})
		if err != nil {
			return written, fmt.Errorf(`failed to read proposals: %w`, err)
		}
		sort.Slice(props, func(i, j int) bool { return props[i].ID < props[j].ID })
		for _, p := range props {
			if err := enc.Encode(exportEnvelope{
				Format: exportFormat, Version: exportVersion, Type: "proposal", Prop: p,
			}); err != nil {
				return written, fmt.Errorf(`failed to write proposal %s: %w`, p.ID, err)
			}
			written++
		}
	}

	if err := bw.Flush(); err != nil {
		return written, err
	}
	return written, nil
}

// ImportJSONL reads an export back into a store. Existing records with the same
// ID are replaced, so an import is a restore rather than a merge.
func ImportJSONL(ctx context.Context, store mecp.Store, r io.Reader) (int, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var imported int
	for line := 1; scanner.Scan(); line++ {
		raw := scanner.Bytes()
		if len(raw) == 0 {
			continue
		}

		var env exportEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return imported, fmt.Errorf(`line %d is not valid JSON: %w`, line, err)
		}
		if env.Format != "" && env.Format != exportFormat {
			return imported, fmt.Errorf(`line %d has unknown format %q`, line, env.Format)
		}

		switch env.Type {
		case "record":
			if env.Record == nil {
				return imported, fmt.Errorf(`line %d declares a record but carries none`, line)
			}
			if err := store.PutRecord(ctx, env.Record); err != nil {
				return imported, fmt.Errorf(`line %d: %w`, line, err)
			}
		case "proposal":
			if env.Prop == nil {
				return imported, fmt.Errorf(`line %d declares a proposal but carries none`, line)
			}
			if _, _, err := store.PutProposal(ctx, env.Prop); err != nil {
				return imported, fmt.Errorf(`line %d: %w`, line, err)
			}
		default:
			return imported, fmt.Errorf(`line %d has unknown entry type %q`, line, env.Type)
		}
		imported++
	}
	if err := scanner.Err(); err != nil {
		return imported, fmt.Errorf(`failed to read export: %w`, err)
	}
	return imported, nil
}
