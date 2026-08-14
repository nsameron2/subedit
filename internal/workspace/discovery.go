package workspace

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/text/cases"

	"subedit/internal/subtitle"
)

var pathFold = cases.Fold()

type candidate struct {
	rel  string
	info fs.FileInfo
}

type discoveryItem struct {
	file  *File
	issue *Issue
}

// Discover recursively indexes supported subtitle files. Parsing is bounded
// and concurrent; returned ordering is deterministic regardless of workers.
func (w *Workspace) Discover(ctx context.Context) (Discovery, error) {
	return w.DiscoverWithProgress(ctx, nil)
}

// DiscoverWithProgress behaves like Discover and reports each supported file
// after it is processed. The callback runs synchronously and serially on the
// collector goroutine, never on parser workers. Total includes both parse
// candidates and issues identified during traversal. Cancellation may produce
// a partial progress sequence, but no callbacks occur after this method
// returns.
func (w *Workspace) DiscoverWithProgress(ctx context.Context, progress func(DiscoveryProgress)) (Discovery, error) {
	if err := w.checkOpen(); err != nil {
		return Discovery{}, err
	}

	candidates, issues, err := w.walkCandidates(ctx)
	if err != nil {
		return Discovery{}, err
	}
	total := len(candidates) + len(issues)
	completed := 0
	report := func(currentPath string, issue *Issue) {
		if progress == nil || ctx.Err() != nil {
			return
		}
		completed++
		var issueCopy *Issue
		if issue != nil {
			copy := *issue
			issueCopy = &copy
		}
		progress(DiscoveryProgress{
			Completed: completed, Total: total, CurrentPath: currentPath, Issue: issueCopy,
		})
	}
	// Traversal issues require no parsing and are already fully classified.
	// Report them before parser results begin so every supported entry accounts
	// for exactly one progress event.
	for index := range issues {
		report(issues[index].RelativePath, &issues[index])
	}

	workers := runtime.GOMAXPROCS(0)
	if workers > 8 {
		workers = 8
	}
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan candidate)
	results := make(chan discoveryItem)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for item := range jobs {
				results <- w.loadCandidate(ctx, item)
			}
		}()
	}
	go func() {
		defer close(results)
		defer group.Wait()
		for _, item := range candidates {
			select {
			case jobs <- item:
			case <-ctx.Done():
				close(jobs)
				return
			}
		}
		close(jobs)
	}()

	files := make([]File, 0, len(candidates))
	for item := range results {
		if item.file != nil {
			files = append(files, *item.file)
			report(item.file.RelativePath, nil)
		} else if item.issue != nil {
			issues = append(issues, *item.issue)
			report(item.issue.RelativePath, item.issue)
		}
	}
	if err := ctx.Err(); err != nil {
		return Discovery{}, err
	}

	sort.Slice(files, func(i, j int) bool { return pathLess(files[i].RelativePath, files[j].RelativePath) })
	sort.Slice(issues, func(i, j int) bool { return pathLess(issues[i].RelativePath, issues[j].RelativePath) })
	return Discovery{
		Root:     w.path,
		Revision: atomic.AddUint64(&w.revision, 1),
		Files:    files,
		Issues:   issues,
	}, nil
}

// DiscoverFile indexes exactly one root-relative subtitle file. It never scans
// siblings or parent directories. Unsupported extensions are rejected; a
// supported target that cannot be safely indexed is returned as one Issue.
func (w *Workspace) DiscoverFile(ctx context.Context, relativePath string, progress func(DiscoveryProgress)) (Discovery, error) {
	if err := w.checkOpen(); err != nil {
		return Discovery{}, err
	}
	rel, err := safeRelative(filepath.ToSlash(relativePath))
	if err != nil {
		return Discovery{}, err
	}
	if !subtitle.SupportedExtension(rel) {
		return Discovery{}, fmt.Errorf("%w: %s", ErrUnsupportedFile, rel)
	}
	if err := ctx.Err(); err != nil {
		return Discovery{}, err
	}

	discovery := Discovery{Root: w.path, Revision: atomic.AddUint64(&w.revision, 1)}
	info, statErr := w.root.Lstat(filepath.FromSlash(rel))
	var item discoveryItem
	if statErr != nil {
		item.issue = &Issue{RelativePath: rel, Kind: IssueUnreadable, Err: statErr}
	} else if info.Mode()&os.ModeSymlink != 0 {
		item.issue = &Issue{RelativePath: rel, Kind: IssueSymlink, Size: info.Size(), Err: ErrUnsafeFile}
	} else if !info.Mode().IsRegular() {
		item.issue = &Issue{RelativePath: rel, Kind: IssueUnsafe, Size: info.Size(), Err: ErrUnsafeFile}
	} else if info.Size() > MaxFileSize {
		item.issue = &Issue{RelativePath: rel, Kind: IssueTooLarge, Size: info.Size(), Err: fs.ErrInvalid}
	} else {
		item = w.loadCandidate(ctx, candidate{rel: rel, info: info})
	}
	if err := ctx.Err(); err != nil {
		return Discovery{}, err
	}
	if item.file != nil {
		discovery.Files = []File{*item.file}
	} else if item.issue != nil {
		discovery.Issues = []Issue{*item.issue}
	}
	if progress != nil {
		var issue *Issue
		if item.issue != nil {
			copy := *item.issue
			issue = &copy
		}
		progress(DiscoveryProgress{Completed: 1, Total: 1, CurrentPath: rel, Issue: issue})
	}
	return discovery, nil
}

func (w *Workspace) walkCandidates(ctx context.Context) ([]candidate, []Issue, error) {
	var candidates []candidate
	var issues []Issue
	err := fs.WalkDir(w.root.FS(), ".", func(entryPath string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entryPath == "." {
			if walkErr != nil {
				return walkErr
			}
			return nil
		}
		rel := entryPath

		if walkErr != nil {
			if subtitle.SupportedExtension(rel) {
				issues = append(issues, Issue{RelativePath: rel, Kind: IssueUnreadable, Err: walkErr})
			}
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if strings.HasPrefix(entry.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !subtitle.SupportedExtension(rel) {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			issues = append(issues, Issue{RelativePath: rel, Kind: IssueSymlink, Err: ErrUnsafeFile})
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			issues = append(issues, Issue{RelativePath: rel, Kind: IssueUnreadable, Err: err})
			return nil
		}
		if !info.Mode().IsRegular() {
			issues = append(issues, Issue{RelativePath: rel, Kind: IssueUnsafe, Size: info.Size(), Err: ErrUnsafeFile})
			return nil
		}
		if info.Size() > MaxFileSize {
			issues = append(issues, Issue{RelativePath: rel, Kind: IssueTooLarge, Size: info.Size(), Err: fs.ErrInvalid})
			return nil
		}
		candidates = append(candidates, candidate{rel: rel, info: info})
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return candidates, issues, nil
}

func (w *Workspace) loadCandidate(ctx context.Context, item candidate) discoveryItem {
	if err := ctx.Err(); err != nil {
		return discoveryItem{issue: &Issue{RelativePath: item.rel, Kind: IssueUnreadable, Err: err}}
	}
	raw, info, identity, err := w.readSafe(item.rel, nil, nil)
	if err != nil {
		kind := IssueUnreadable
		if errors.Is(err, ErrHardlink) {
			kind = IssueHardlink
		} else if errors.Is(err, ErrUnsafeFile) || errors.Is(err, ErrUnsafePath) {
			kind = IssueUnsafe
		}
		return discoveryItem{issue: &Issue{RelativePath: item.rel, Kind: kind, Size: item.info.Size(), Err: err}}
	}
	document, err := subtitle.Parse(item.rel, raw)
	if err != nil {
		return discoveryItem{issue: &Issue{RelativePath: item.rel, Kind: IssueInvalid, Size: info.Size(), Err: err}}
	}
	return discoveryItem{file: &File{
		RelativePath: item.rel,
		Size:         info.Size(),
		Mode:         info.Mode(),
		ModTime:      info.ModTime(),
		SHA256:       sha256.Sum256(raw),
		Identity:     identity,
		Document:     document,
	}}
}

// Search evaluates a literal normalized query against a discovery snapshot.
func (d Discovery) Search(query string) SearchResult {
	return d.SearchAny([]string{query})
}

// SearchAny evaluates phrases with OR semantics. Empty normalized phrases and
// normalized duplicates are ignored. Each cue appears once, with CueMatches
// identifying every effective phrase that matched it.
func (d Discovery) SearchAny(queries []string) SearchResult {
	result := SearchResult{
		Revision:     d.Revision,
		TotalFiles:   len(d.Files),
		SkippedFiles: len(d.Issues),
	}
	seen := make(map[string]struct{}, len(queries))
	for _, query := range queries {
		normalized := subtitle.NormalizeQuery(query)
		if normalized == "" {
			continue
		}
		if _, duplicate := seen[normalized]; duplicate {
			continue
		}
		seen[normalized] = struct{}{}
		result.Queries = append(result.Queries, query)
		result.NormalizedQueries = append(result.NormalizedQueries, normalized)
	}
	if len(result.Queries) > 0 {
		result.Query = result.Queries[0]
		result.NormalizedQuery = result.NormalizedQueries[0]
	}
	if len(result.NormalizedQueries) == 0 {
		return result
	}
	for _, file := range d.Files {
		matched := make(map[subtitle.CueID][]int)
		for queryIndex, normalized := range result.NormalizedQueries {
			for _, cue := range file.Document.Search(normalized) {
				matched[cue.ID] = append(matched[cue.ID], queryIndex)
			}
		}
		if len(matched) == 0 {
			continue
		}
		match := FileMatch{File: file}
		for _, cue := range file.Document.Cues {
			indexes, ok := matched[cue.ID]
			if !ok {
				continue
			}
			match.Cues = append(match.Cues, cue)
			match.CueMatches = append(match.CueMatches, CueMatch{Cue: cue, QueryIndexes: indexes})
		}
		result.MatchingCues += len(match.Cues)
		result.Matches = append(result.Matches, match)
	}
	return result
}

func pathLess(left, right string) bool {
	l, r := pathFold.String(filepath.ToSlash(left)), pathFold.String(filepath.ToSlash(right))
	if l == r {
		return filepath.ToSlash(left) < filepath.ToSlash(right)
	}
	return l < r
}
