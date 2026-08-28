package seestar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/glennbech/pancakestack-cli/internal/api"
)

// heartbeatInterval — how often the sync loop pings the backend so the
// webapp can render a "Live import in progress" banner. Must be strictly
// shorter than the server's liveImportWindow (currently 15s) so a
// healthy sync never flickers out; 5s leaves comfortable slack for a
// slow round-trip.
const heartbeatInterval = 5 * time.Second

// liveSnapshot is the last-known state the heartbeat ticker sends. The
// main sync loop overwrites it (atomic swap) whenever a new scope
// listing lands; the ticker just reads and posts.
type liveSnapshot struct {
	folder    string
	filesSeen int
}

// SyncOptions drives one continuous sync session (scope → collection).
type SyncOptions struct {
	// Device is the scope to pull from — usually the result of a
	// Discover() call.
	Device Device
	// KeyPath is the RSA PEM for JSON-RPC auth (may be ""; see
	// NewClient). Passing an empty string on ≥7.18 firmware means
	// the first Call will error with a helpful message.
	KeyPath string
	// Folder is the scope-side folder under MyWorks/ to sync from.
	// Case + spaces matter (`M 81_sub`).
	Folder string
	// CollectionID is the pancakestack collection to upload into.
	CollectionID string
	// Interval between listing polls. Default 30s. Under ~10s risks
	// racing the scope's own write of a still-active frame — the
	// two-poll size-stable guard mitigates but doesn't remove that.
	Interval time.Duration
	// Concurrency of the S3 upload phase. Default 4 — same as the
	// standard `upload` verb.
	Concurrency int
	// StackWhen: once this many NEW files have been uploaded during
	// this session, fire one POST /jobs and clear the trigger. 0 =
	// never auto-stack. -1 = disabled (same as 0). Positive values
	// count only uploads that happened in this run — restart-and-
	// resume doesn't re-fire the stack.
	StackWhen int
	// StackScriptID is the catalog script to run when StackWhen fires.
	// Empty → backend picks the default.
	StackScriptID string
	// StackParams are the catalog params to pass on the auto-stack.
	StackParams map[string]any
	// Client is the backend API client (already authenticated).
	Client *api.Client
	// State stores which files have already been uploaded across
	// process restarts.
	State *State
	// FromNow, when true on a first-run for this (serial, folder),
	// baselines every file currently on the scope as "already seen"
	// and only uploads frames that appear on subsequent polls. Ignored
	// once state has entries — you can't un-see files.
	FromNow bool
	// Backfill is the opt-in for the classic behavior: on first run,
	// upload every FITS the scope already holds. Required (with
	// FromNow as the alternative) to prevent accidental multi-GB
	// downloads of a folder full of previous nights' subs.
	Backfill bool
	// Log lets the caller decide where progress lines go. Stderr is
	// the usual choice; nil silences output.
	Log func(format string, args ...any)
}

// Run drives one sync session. Blocks until ctx is cancelled or an
// unrecoverable error surfaces. Returns nil on graceful shutdown.
func Run(ctx context.Context, opts SyncOptions) error {
	if opts.Interval == 0 {
		opts.Interval = 30 * time.Second
	}
	if opts.Concurrency == 0 {
		opts.Concurrency = 4
	}
	if opts.Log == nil {
		opts.Log = func(string, ...any) {}
	}
	if opts.CollectionID == "" || opts.Folder == "" {
		return fmt.Errorf("collectionId and folder are both required")
	}

	rpc := NewClient(opts.Device.IP, opts.KeyPath)
	defer rpc.Close()

	// Fail fast if we can't reach the scope at all — otherwise the
	// first poll cycle would be a silent 30s wait for nothing.
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err := rpc.Ping(pingCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("cannot reach scope at %s: %w", opts.Device.IP, err)
	}
	opts.Log("connected: %s (%s) at %s\n", opts.Device.Model, opts.Device.Serial, opts.Device.IP)

	// First-run gate: if state has zero entries for this folder,
	// force the caller to pick between --from-now (skip existing) and
	// --backfill (upload everything). Without this, a fresh sync of a
	// folder that already contains a night's worth of subs silently
	// downloads and uploads all of them — the exact regression that
	// prompted this flag.
	firstRun := opts.State.CountFolder(opts.Folder) == 0
	if firstRun && !opts.FromNow && !opts.Backfill {
		return fmt.Errorf(
			"first sync of folder %q — pass --from-now to skip files "+
				"already on the scope and only upload new frames, or "+
				"--backfill to upload everything already there",
			opts.Folder,
		)
	}
	if firstRun && opts.FromNow {
		n, err := baselineExisting(ctx, rpc, opts)
		if err != nil {
			return fmt.Errorf("baseline: %w", err)
		}
		opts.Log("· baseline: marked %d existing file(s) as already-seen (--from-now)\n", n)
	}

	// Reconcile local state against the collection's current contents.
	// The rule: once a file lands in a collection, we never re-upload
	// it — even if the user later deletes it during QA. Local state
	// enforces that going forward, but a fresh install (or a sync
	// session whose Mark was lost to a crash) has gaps: files that
	// reached the backend but never made it into state, so the loop
	// treats them as new and re-uploads. Reconcile closes that hole
	// by seeding state from the backend on every start-up. Best-effort:
	// a failed reconcile logs quietly and the loop runs anyway (the
	// user just may see one extra upload cycle before state converges).
	if opts.Client != nil && opts.CollectionID != "" {
		n, err := reconcileFromCollection(ctx, rpc, opts)
		if err != nil {
			opts.Log("· reconcile skipped: %v\n", err)
		} else if n > 0 {
			opts.Log("· reconcile: added %d already-in-collection file(s) to state\n", n)
		}
	}

	// Heartbeat ticker — every 5s post the last-known scope listing
	// snapshot to the backend so the webapp can render the "Live import"
	// banner. Best-effort: a failed heartbeat logs quietly and the loop
	// keeps going (the sync itself doesn't depend on it). The atomic
	// swap decouples the ticker's cadence from the poll cadence.
	var snap atomic.Pointer[liveSnapshot]
	snap.Store(&liveSnapshot{folder: opts.Folder})
	go runHeartbeat(ctx, opts.Client, opts.CollectionID, &snap, opts.Log)

	// lastSize is the previous-poll size for every file we've seen
	// mid-flight but not yet uploaded. Two consecutive polls with
	// identical size = safe to download.
	lastSize := map[string]int64{}
	// uploadedThisRun counts NEW uploads this session — feeds the
	// StackWhen trigger.
	uploadedThisRun := 0
	stackFired := false

	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		files, err := rpc.ListFiles(ctx, opts.Folder)
		if err != nil {
			opts.Log("list %q failed: %v (will retry)\n", opts.Folder, err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(opts.Interval):
			}
			continue
		}
		snap.Store(&liveSnapshot{folder: opts.Folder, filesSeen: len(files)})

		// Compute the set of files that (a) aren't in state yet and
		// (b) have a stable size between this poll and the last.
		ready := make([]File, 0, len(files))
		for _, f := range files {
			if f.Size == 0 {
				continue
			}
			if opts.State.Has(opts.Folder, f.Name, f.Size) {
				continue
			}
			key := f.Name
			prev, seen := lastSize[key]
			lastSize[key] = f.Size
			if !seen {
				continue // first sighting — wait one poll for size to settle
			}
			if prev != f.Size {
				continue // still growing on the scope
			}
			ready = append(ready, f)
		}

		if len(ready) == 0 {
			opts.Log("· idle (folder=%q, %d file(s) on scope, none new to upload)\n", opts.Folder, len(files))
		} else {
			n, err := opts.uploadReady(ctx, rpc, ready)
			if err != nil {
				opts.Log("upload cycle failed: %v (will retry next poll)\n", err)
			} else {
				uploadedThisRun += n
			}
		}

		// Auto-stack trigger.
		if !stackFired && opts.StackWhen > 0 && uploadedThisRun >= opts.StackWhen {
			opts.Log("· stack trigger reached (%d new frames uploaded)\n", uploadedThisRun)
			if err := opts.launchStack(ctx); err != nil {
				opts.Log("auto-stack launch failed: %v (continuing sync)\n", err)
			}
			stackFired = true // only once per process
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(opts.Interval):
		}
	}
}

// uploadReady downloads each file in `ready` to a per-cycle tempdir
// and hands the batch to the CLI's existing bulk-upload primitive.
// Returns the number of files uploaded (excludes duplicates the
// backend rejected).
func (o SyncOptions) uploadReady(ctx context.Context, rpc *Client, ready []File) (int, error) {
	tmpDir, err := os.MkdirTemp("", "seestar-sync-*")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(tmpDir)

	paths := make([]string, 0, len(ready))
	shas := make([]string, 0, len(ready))
	sizes := make(map[string]int64, len(ready))

	for _, f := range ready {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		dst := filepath.Join(tmpDir, f.Name)
		o.Log("↓ %s (%.1f MiB) ...\n", f.Name, float64(f.Size)/1024/1024)
		sum, err := downloadToWithSHA(ctx, o.Device.IP, o.Folder, f.Name, dst)
		if err != nil {
			o.Log("  download failed: %v — skipping this cycle\n", err)
			continue
		}
		paths = append(paths, dst)
		shas = append(shas, sum)
		sizes[f.Name] = f.Size
	}
	if len(paths) == 0 {
		return 0, nil
	}

	o.Log("↑ uploading %d file(s) → collection %q ...\n", len(paths), o.CollectionID)
	results, err := o.Client.UploadFilesBulk(ctx, api.BulkUploadOptions{
		CollectionID: o.CollectionID,
		Paths:        paths,
		SHA256s:      shas,
		Concurrency:  o.Concurrency,
	})
	if err != nil {
		return 0, err
	}
	uploaded := 0
	for _, r := range results {
		if r.Error != nil {
			o.Log("  ✗ %s: %v\n", r.Name, r.Error)
			continue
		}
		// Mark both real uploads AND duplicates in state — a duplicate
		// means the backend already has it (from an earlier session,
		// re-upload, etc.), so future polls should stop offering it.
		markErr := o.State.Mark(SyncEntry{
			Serial:     o.Device.Serial,
			Folder:     o.Folder,
			Filename:   r.Name,
			Size:       sizes[r.Name],
			Collection: o.CollectionID,
			UploadedAt: time.Now().UTC(),
		})
		if markErr != nil {
			o.Log("  state write failed for %s: %v\n", r.Name, markErr)
		}
		if r.Skipped {
			o.Log("  · %s: already in collection\n", r.Name)
		} else {
			uploaded++
		}
	}
	o.Log("· cycle done: %d uploaded, %d duplicates\n", uploaded, len(results)-uploaded)
	return uploaded, nil
}

// launchStack posts POST /jobs for the collection using the caller's
// script + params. Fires once per process — StackWhen is a trigger,
// not a repeated schedule.
func (o SyncOptions) launchStack(ctx context.Context) error {
	resp, err := o.Client.Stack(ctx, api.StackRequest{
		CollectionID: o.CollectionID,
		ScriptID:     o.StackScriptID,
		Params:       o.StackParams,
	})
	if err != nil {
		return err
	}
	o.Log("✓ stack launched: jobId=%s script=%s\n", resp.JobID, resp.ScriptID)
	return nil
}

// runHeartbeat keeps posting the last-known scope snapshot to the
// backend on a fixed cadence so the webapp's "Live import" banner
// stays lit for the whole session. Fires immediately (so the banner
// shows within one webapp poll after `seestar sync` starts, not one
// heartbeatInterval later), then on every tick. Exits when ctx is
// cancelled. Heartbeat failures are logged but not fatal — a missed
// beat just means the banner briefly clears until the next tick.
func runHeartbeat(
	ctx context.Context,
	client *api.Client,
	collectionID string,
	snap *atomic.Pointer[liveSnapshot],
	logf func(string, ...any),
) {
	beat := func() {
		s := snap.Load()
		if s == nil {
			return
		}
		// Give the backend a short deadline of its own — the sync loop's
		// ctx is long-lived, so on a slow network we don't want a
		// heartbeat to pile up behind another. 3s is well under the
		// tick interval.
		hbCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if err := client.LiveImportHeartbeat(hbCtx, collectionID, s.folder, s.filesSeen); err != nil {
			// Quiet log — if the CLI can't reach the API at all the
			// user will see the sync loop's own upload errors first,
			// so we don't need to shout.
			logf("· heartbeat failed: %v\n", err)
		}
	}
	beat()
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			beat()
		}
	}
}

// reconcileFromCollection cross-references the target collection's
// current contents against what the scope currently holds in the sync
// folder. For every filename present in both, seed a local state
// entry so future polls skip it — ensures files uploaded via other
// channels (webapp uploads, another machine, a prior sync session
// whose state was lost) are treated as already-seen, so a QA-delete
// on the webapp doesn't cause the deleted file to churn back in.
//
// Match is by filename only: Seestar filenames encode target + exposure
// + filter + UTC timestamp, so identical names are effectively identical
// files. The stored size is the SCOPE's reported size (SizeK*1024),
// not the backend's exact-byte SizeBytes, so state.Has() on future
// polls always matches (they compare the same rounded value).
func reconcileFromCollection(ctx context.Context, rpc *Client, opts SyncOptions) (int, error) {
	collectionFiles, err := opts.Client.ListCollectionFiles(ctx, opts.CollectionID)
	if err != nil {
		return 0, err
	}
	if len(collectionFiles) == 0 {
		return 0, nil
	}
	inCollection := make(map[string]struct{}, len(collectionFiles))
	for _, f := range collectionFiles {
		if f.Name != "" {
			inCollection[f.Name] = struct{}{}
		}
	}
	scopeFiles, err := rpc.ListFiles(ctx, opts.Folder)
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	entries := make([]SyncEntry, 0, len(scopeFiles))
	for _, sf := range scopeFiles {
		if sf.Size == 0 {
			continue
		}
		if _, ok := inCollection[sf.Name]; !ok {
			continue
		}
		if opts.State.Has(opts.Folder, sf.Name, sf.Size) {
			continue
		}
		entries = append(entries, SyncEntry{
			Serial:     opts.Device.Serial,
			Folder:     opts.Folder,
			Filename:   sf.Name,
			Size:       sf.Size,
			Collection: opts.CollectionID,
			UploadedAt: now,
		})
	}
	if len(entries) == 0 {
		return 0, nil
	}
	if err := opts.State.MarkBatch(entries); err != nil {
		return 0, err
	}
	return len(entries), nil
}

// baselineExisting lists the folder once and records every current
// file as already-uploaded, without downloading anything. Used by
// --from-now so a fresh sync only picks up frames that appear on
// subsequent polls — avoids re-pulling a folder full of previous
// nights' subs. Files still growing on the scope (unstable size)
// are baselined too; that's fine because their next-poll size will
// differ from the baselined value and the state check falls through
// to a normal upload.
func baselineExisting(ctx context.Context, rpc *Client, opts SyncOptions) (int, error) {
	files, err := rpc.ListFiles(ctx, opts.Folder)
	if err != nil {
		return 0, err
	}
	if len(files) == 0 {
		return 0, nil
	}
	now := time.Now().UTC()
	entries := make([]SyncEntry, 0, len(files))
	for _, f := range files {
		if f.Size == 0 {
			continue
		}
		entries = append(entries, SyncEntry{
			Serial:     opts.Device.Serial,
			Folder:     opts.Folder,
			Filename:   f.Name,
			Size:       f.Size,
			Collection: "", // baseline entries have no collection
			UploadedAt: now,
		})
	}
	if err := opts.State.MarkBatch(entries); err != nil {
		return 0, err
	}
	return len(entries), nil
}

// downloadToWithSHA streams one file to disk and returns its
// lowercase-hex SHA-256 in the same pass — so the sync loop doesn't
// need to re-read the file just to hash it for /upload/presign.
func downloadToWithSHA(ctx context.Context, ip, folder, filename, dst string) (string, error) {
	f, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	mw := io.MultiWriter(f, h)
	if _, err := DownloadFile(ctx, ip, folder, filename, mw); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
