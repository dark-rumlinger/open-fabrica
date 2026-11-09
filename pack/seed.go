package pack

import (
	"sync"

	"openfabrica/knowledge"
)

// Embedded parses and returns every embedded platform pack - the capability
// guides (knowledge/guides/**) and the stage frames + fragments
// (knowledge/stages/**) - sorted by (position, slug). This is the source of
// truth the engine assembles from; there is no DB overlay. Deterministic.
//
// The embedded tree never changes at runtime, so the parse is done once and
// cached. Each call returns a fresh copy of the slice so a caller can sort or
// filter it without disturbing the cache.
func Embedded() ([]Pack, error) {
	embeddedOnce.Do(func() {
		embeddedCache, embeddedErr = loadEmbedded()
	})
	if embeddedErr != nil {
		return nil, embeddedErr
	}
	out := make([]Pack, len(embeddedCache))
	copy(out, embeddedCache)
	return out, nil
}

var (
	embeddedOnce  sync.Once
	embeddedCache []Pack
	embeddedErr   error
)

func loadEmbedded() ([]Pack, error) {
	guides, err := ParseFS(knowledge.FS, "guides")
	if err != nil {
		return nil, err
	}
	stages, err := ParseFS(knowledge.FS, "stages")
	if err != nil {
		return nil, err
	}
	out := append(guides, stages...)
	sortPacks(out)
	return out, nil
}
