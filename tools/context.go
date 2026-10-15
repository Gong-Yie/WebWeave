package tools

import (
	"errors"

	"webweave/workspace"
)

func (e *Executor) readContextSegment(id string, args Args) (any, error) {
	maximum := 20000
	if args.MaxChars != nil {
		maximum = *args.MaxChars
	}
	if maximum < 1 || maximum > 100000 || args.Offset < 0 {
		return nil, errors.New("offset 必须非负，max_chars 必须在 1 到 100000 之间")
	}
	segment, err := workspace.ReadContextSegment(e.root, id, args.SegmentID)
	if err != nil {
		return nil, err
	}
	runes := []rune(segment.Original)
	if args.Offset > len(runes) {
		return nil, errors.New("offset 超过原文长度")
	}
	end := min(len(runes), args.Offset+maximum)
	return map[string]any{"segment_id": segment.ID, "kind": segment.Kind, "source": segment.Source, "content": string(runes[args.Offset:end]), "offset": args.Offset, "next_offset": end, "total_chars": len(runes), "truncated": end < len(runes)}, nil
}
