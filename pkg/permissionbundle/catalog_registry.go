package permissionbundle

import (
	"bytes"
	"context"
	"sync/atomic"
)

// SupportedSource is a private reviewed support input. Format selects exactly
// one representation. A valid package/manifest cannot add itself to this list.
type SupportedSource struct {
	Pin      CatalogPin
	Format   string
	Artifact []byte
	Files    map[string][]byte
}
type catalogRecord struct {
	format      string
	release     Release
	packageView *VerifiedCatalog
	semantic    []byte
}
type catalogSet struct {
	records  map[CatalogPin]catalogRecord
	pairs    map[CatalogTransition]bool
	versions map[string]string
}

// CatalogRegistry implements Releases and private file reads with one atomic,
// whole-registry publication. A failed final file/pair leaves the old set intact.
type CatalogRegistry struct{ current atomic.Pointer[catalogSet] }

func NewCatalogRegistry(ctx context.Context, sources []SupportedSource, pairs []CatalogTransition, slots SlotValues) (*CatalogRegistry, error) {
	r := &CatalogRegistry{}
	if e := r.Replace(ctx, sources, pairs, slots); e != nil {
		return nil, e
	}
	return r, nil
}

func (r *CatalogRegistry) Replace(ctx context.Context, sources []SupportedSource, pairs []CatalogTransition, slots SlotValues) error {
	if r == nil || ctx.Err() != nil || len(sources) == 0 {
		return ErrUnavailable
	}
	previous := r.current.Load()
	next := &catalogSet{map[CatalogPin]catalogRecord{}, map[CatalogTransition]bool{}, map[string]string{}}
	if previous != nil {
		for version, digest := range previous.versions {
			next.versions[version] = digest
		}
	}
	seenVersions := map[string]bool{}
	for _, source := range sources {
		if seenVersions[source.Pin.Version] || next.versions[source.Pin.Version] != "" && next.versions[source.Pin.Version] != source.Pin.Digest || ctx.Err() != nil {
			return ErrUnavailable
		}
		record := catalogRecord{format: source.Format}
		switch source.Format {
		case CatalogArtifactV1:
			if source.Files != nil {
				return ErrUnavailable
			}
			loader, e := NewArtifactReleases([]SupportedCatalog{{source.Pin, source.Artifact}}, nil)
			if e != nil {
				return e
			}
			record.release, e = loader.Load(ctx, source.Pin)
			if e != nil {
				return e
			}
		case CatalogPackageV2:
			if len(source.Artifact) != 0 {
				return ErrUnavailable
			}
			v, e := VerifyCatalogPackage(ctx, PackageInput{source.Pin, source.Files}, slots)
			if e != nil {
				return e
			}
			record.packageView = v
			record.semantic = v.semantic
			caps := map[string]bool{}
			for id := range v.entries {
				caps[id] = true
			}
			record.release = Release{source.Pin, v.files["manifest.json"], caps}
		default:
			return ErrVersion
		}
		next.records[source.Pin] = record
		next.versions[source.Pin.Version] = source.Pin.Digest
		seenVersions[source.Pin.Version] = true
	}
	for _, pair := range pairs {
		from, ok := next.records[pair.From]
		to, ok2 := next.records[pair.To]
		if !ok || !ok2 || pair.From == pair.To || next.pairs[pair] {
			return ErrUnavailable
		}
		for _, pin := range []CatalogPin{pair.From, pair.To} {
			record := next.records[pin]
			if record.semantic == nil {
				v, e := normalizeArtifactV1(ctx, record.release, slots)
				if e != nil {
					return e
				}
				record.semantic = v.semantic
				next.records[pin] = record
			}
		}
		from = next.records[pair.From]
		to = next.records[pair.To]
		// Hashes accelerate comparison; complete validated view equality is also
		// required. The approval list cannot override changed semantic content.
		if hash(from.semantic) != hash(to.semantic) || !bytes.Equal(from.semantic, to.semantic) {
			return packageError("directed pair changes semantic view")
		}
		next.pairs[pair] = true
	}
	if ctx.Err() != nil {
		return ErrUnavailable
	}
	if !r.current.CompareAndSwap(previous, next) {
		return ErrConflict
	}
	return nil
}

func (r *CatalogRegistry) record(ctx context.Context, pin CatalogPin) (catalogRecord, error) {
	if r == nil || ctx.Err() != nil {
		return catalogRecord{}, ErrUnavailable
	}
	set := r.current.Load()
	if set == nil {
		return catalogRecord{}, ErrUnavailable
	}
	record, ok := set.records[pin]
	if !ok {
		return catalogRecord{}, ErrUnavailable
	}
	return record, nil
}
func (r *CatalogRegistry) Load(ctx context.Context, pin CatalogPin) (Release, error) {
	record, e := r.record(ctx, pin)
	if e != nil {
		return Release{}, e
	}
	s := record.release
	copy := Release{s.Pin, bytes.Clone(s.Bytes), map[string]bool{}}
	for id := range s.Capabilities {
		copy.Capabilities[id] = true
	}
	return copy, nil
}
func (r *CatalogRegistry) CatalogFormat(ctx context.Context, pin CatalogPin) (string, error) {
	record, e := r.record(ctx, pin)
	return record.format, e
}
func (r *CatalogRegistry) CatalogFile(ctx context.Context, pin CatalogPin, path string) ([]byte, error) {
	record, e := r.record(ctx, pin)
	if e != nil || record.packageView == nil {
		return nil, ErrUnavailable
	}
	if _, _, _, ok := packagePath(path); !ok {
		return nil, ErrInvalid
	}
	b, ok := record.packageView.File(path)
	if !ok {
		return nil, ErrUnavailable
	}
	return b, nil
}
func (r *CatalogRegistry) Compatible(ctx context.Context, from, to CatalogPin) error {
	if r == nil || ctx.Err() != nil {
		return ErrUnavailable
	}
	set := r.current.Load()
	if set == nil || set.records[from].format == "" || set.records[to].format == "" || from != to && !set.pairs[CatalogTransition{from, to}] {
		return ErrUnavailable
	}
	return nil
}
func (r *CatalogRegistry) SameIntent(ctx context.Context, from, to CatalogPin, rule Rule) (bool, error) {
	if e := r.Compatible(ctx, from, to); e != nil {
		return false, e
	}
	caps, ok := rule["capabilities"].([]any)
	if !ok || len(caps) == 0 || rule["effect"] != "deny" {
		return false, ErrInvalid
	}
	// A successful directed pair already compared the complete semantic table.
	set := r.current.Load()
	if set == nil || set.records[from].format == "" || set.records[to].format == "" || from != to && !set.pairs[CatalogTransition{from, to}] {
		return false, ErrUnavailable
	}
	for _, value := range caps {
		id, ok := value.(string)
		if !ok || !set.records[from].release.Capabilities[id] || !set.records[to].release.Capabilities[id] {
			return false, nil
		}
	}
	return true, nil
}

type catalogFileReader interface {
	CatalogFormat(context.Context, CatalogPin) (string, error)
	CatalogFile(context.Context, CatalogPin, string) ([]byte, error)
}

func catalogFormat(ctx context.Context, releases Releases, pin CatalogPin) (string, error) {
	if reader, ok := releases.(catalogFileReader); ok {
		format, e := reader.CatalogFormat(ctx, pin)
		if e != nil || format != CatalogArtifactV1 && format != CatalogPackageV2 {
			return "", ErrUnavailable
		}
		return format, nil
	}
	return CatalogArtifactV1, nil // unchanged legacy Releases integration
}
