package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
)

// ContextSegment keeps exact observations separate from the model's preview.
type ContextSegment struct {
	ID         string  `json:"id"`
	Kind       string  `json:"kind"`
	Source     string  `json:"source"`
	Original   string  `json:"original"`
	Compressed string  `json:"compressed,omitempty"`
	Model      string  `json:"model,omitempty"`
	Rate       float64 `json:"rate,omitempty"`
}

var segmentIDPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func contextSegmentPath(root, id, segmentID string) (string, error) {
	if !ValidName(id) || !segmentIDPattern.MatchString(segmentID) {
		return "", errors.New("运行 ID 或上下文片段 ID 无效")
	}
	return ResolveWithin(ScopeRoot(root, "run", id), filepath.Join("context_segments", segmentID+".json"), false)
}

func ReadContextSegment(root, id, segmentID string) (ContextSegment, error) {
	var segment ContextSegment
	path, err := contextSegmentPath(root, id, segmentID)
	if err != nil {
		return segment, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return segment, err
	}
	if err := json.Unmarshal(data, &segment); err != nil {
		return segment, err
	}
	if segment.ID != segmentID {
		return segment, errors.New("上下文片段 ID 不匹配")
	}
	return segment, nil
}

func SaveContextSegment(root, id, kind, source, original string) (ContextSegment, error) {
	hash := sha256.Sum256([]byte(kind + "\x00" + source + "\x00" + original))
	segmentID := hex.EncodeToString(hash[:])
	segment, err := ReadContextSegment(root, id, segmentID)
	if err == nil {
		return segment, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return segment, err
	}
	segment = ContextSegment{ID: segmentID, Kind: kind, Source: source, Original: original}
	return segment, UpdateContextSegment(root, id, segment)
}

func UpdateContextSegment(root, id string, segment ContextSegment) error {
	path, err := contextSegmentPath(root, id, segment.ID)
	if err != nil {
		return err
	}
	return WriteJSONAtomic(path, segment)
}
