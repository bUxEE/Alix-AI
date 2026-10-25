package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/upgrade"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Repair of one derived index outside an upgrade (master plan 18 "SQLite/index corruption: isolate
// damage, restore metadata backup or rebuild derived index", 19; M5-25, the gap (b) of the M6-04
// drill).
//
//	alixai index repair -index NAME [-config FILE | -data-dir DIR]
//
// `upgrade apply` repairs every derived index as a step of an upgrade and `upgrade recover` rebuilds
// what is already isolated; neither is a way to repair one index on a deployment that is not being
// upgraded. This command does the same three steps for exactly one index, with the same pieces
// (internal/upgrade), in the same order:
//
//  1. check it. A healthy index that is not isolated is left alone ("healthy"): repairing is not
//     rebuilding, and a vector index cannot be rebuilt without dropping every vector.
//  2. isolate it: record it in the quarantine (<data dir>/upgrade/quarantine) BEFORE its contents
//     are dropped, so a crash between the two leaves a record and never an emptied index that
//     nothing says is empty. A running service labels what an isolated index cannot answer
//     (retrieval: reduced_semantic_recall; history search: unavailable) from the moment the record
//     exists.
//  3. rebuild it from its source, check it again and release the record. A full-text index is
//     rebuilt by SQLite from its content table. A vector index is the output of the embedding
//     model, so it cannot be rebuilt here (the M5-24 rule): it stays isolated, the outcome is
//     "pending", and `alixai serve` re-embeds it and releases the record when an embedder role is
//     registered and rag.embedding.model is set.
//
// A record left by an interrupted repair, or by `upgrade apply`, is finished the same way: step 3.
//
// It must not call upgrade.RepairDerived or RebuildQuarantined with only the one index: both release
// every quarantine record they were not handed an index for (they read it as an index a migration
// dropped), which would silently release another index's record while it is still empty
// (internal/upgrade/reembed.go). Only the record of the named index is ever added or cleared here.
// The service need not be stopped: the store serialises the writes, and the record is what the
// service reads.

const opsInspectIndexUsage = "alixai index repair -index NAME [-config FILE | -data-dir DIR]"

func init() {
	Register(Command{
		Name:    "index repair",
		Summary: "isolate, rebuild and verify one derived index (full-text, or vector: re-embedded by serve) outside an upgrade",
		Run:     opsInspectRunIndexRepair,
	})
}

// opsInspectRepair is what a repair did to one index.
type opsInspectRepair struct {
	Index string `json:"index"`
	// Outcome is "healthy" (nothing to do), "failed" (the error says why), "rebuilt" (isolated if it was damaged, rebuilt, checked and
	// released) or "pending" (isolated, waiting for the embedder: a vector index).
	Outcome string `json:"outcome"`
	// Damaged is true when this run found the index damaged and isolated it; Resumed is true when it
	// finished the repair of an index that was already isolated.
	Damaged bool   `json:"damaged"`
	Resumed bool   `json:"resumed"`
	Detail  string `json:"detail,omitempty"`
}

// opsInspectRepairOne repairs ix and no other index of q.
func opsInspectRepairOne(ctx context.Context, q *upgrade.Quarantine, ix upgrade.DerivedIndex) (opsInspectRepair, error) {
	name := ix.Name()
	res := opsInspectRepair{Index: name}
	if q.Has(name) {
		res.Resumed = true
	} else {
		switch err := ix.Check(ctx); {
		case err == nil:
			res.Outcome = "healthy"
			return res, nil
		case !errors.Is(err, upgrade.ErrDamaged):
			return res, fmt.Errorf("index %s: the check could not be made: %w", name, err)
		default:
			res.Damaged = true
			res.Detail = err.Error()
			if err := q.Add(name, err.Error()); err != nil {
				return res, err
			}
			if err := ix.Isolate(ctx); err != nil {
				return res, opsInspectStructural(name, err)
			}
		}
	}
	if err := ix.Rebuild(ctx); err != nil {
		if errors.Is(err, upgrade.ErrReembedRequired) {
			res.Outcome = "pending"
			res.Detail = "isolated: the vectors are made by the embedding model, so alixai serve re-embeds them and releases the record once an embedder role and rag.embedding.model are configured; until then retrieval is full-text only and labelled reduced_semantic_recall"
			return res, nil
		}
		return res, opsInspectStructural(name, err)
	}
	if err := ix.Check(ctx); err != nil {
		return res, opsInspectStructural(name, err)
	}
	if err := q.Clear(name); err != nil {
		return res, err
	}
	res.Outcome = "rebuilt"
	return res, nil
}

// opsInspectStructural is a rebuild that failed or an index that still fails its check: the damage
// is in the file itself, the record stays, and the way out is the last backup.
func opsInspectStructural(name string, cause error) error {
	return contracts.Wrap(contracts.ErrInternal, cause,
		"derived index "+name+" cannot be rebuilt in place; it stays isolated: the damage is structural, restore the last backup (backup restore)").
		WithDetails("index", name)
}

func opsInspectRunIndexRepair(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("index repair")
	name := fs.String("index", "", "the derived index to repair, e.g. history_fts or vector_rows (required)")
	dataDirFlag := fs.String("data-dir", "", "data directory (default: from the configuration)")
	cfgPath := env.ConfigFlag(fs)
	if err := env.ParseFlags(fs, opsInspectIndexUsage, args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *name == "" {
		return Usagef("-index is required (usage: %s)", opsInspectIndexUsage)
	}
	dataDir, err := backupDataDir(env, *cfgPath, *dataDirFlag)
	if err != nil {
		return err
	}
	// Opened as backup does: no migration is applied, a missing database is an error and not a new file.
	db, err := backupOpenStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer db.Close()

	idx, err := upgrade.FTSIndexes(ctx, db)
	if err != nil {
		return err
	}
	idx = append(idx, upgrade.VectorDerived(nil)(db)...)
	var names []string
	var found upgrade.DerivedIndex
	for _, ix := range idx {
		names = append(names, ix.Name())
		if ix.Name() == *name {
			found = ix
		}
	}
	if found == nil {
		sort.Strings(names)
		return Usagef("-index %q is not a derived index of this database (indexes: %s)", *name, strings.Join(names, ", "))
	}
	res, err := opsInspectRepairOne(ctx, upgrade.NewQuarantine(dataDir, clock.Real{}), found)
	if err != nil && res.Outcome == "" {
		res.Outcome = "failed"
		res.Detail = err.Error()
	}
	if eerr := backupEmit(env.Stdout, res); eerr != nil && err == nil {
		err = eerr
	}
	return err
}
