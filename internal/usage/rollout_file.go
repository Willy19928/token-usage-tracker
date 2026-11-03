package usage

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Codex later replaces old rollouts with a zstd-compressed copy named
// <rollout>.jsonl.zst. The scanner keeps addressing such a rollout by its
// original .jsonl path, so cursors, sessions and event origins stay attached to
// the same logical file and reading simply continues at the saved offset in
// the decompressed content.
const compressedRolloutSuffix = ".zst"

// logicalRolloutPath maps a directory entry to the rollout path the scanner
// tracks. A compressed copy is tracked under its .jsonl name unless the plain
// file still exists, which then wins.
func logicalRolloutPath(path string) (string, bool) {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".jsonl"):
		return path, true
	case strings.HasSuffix(lower, ".jsonl"+compressedRolloutSuffix):
		plain := path[:len(path)-len(compressedRolloutSuffix)]
		if _, err := os.Stat(plain); errors.Is(err, os.ErrNotExist) {
			return plain, true
		}
	}
	return "", false
}

// rolloutInfo describes a compressed rollout by its decompressed size.
type rolloutInfo struct {
	os.FileInfo
	size int64
}

func (i rolloutInfo) Size() int64 { return i.size }

func isCompressedRollout(info os.FileInfo) bool {
	_, ok := info.(rolloutInfo)
	return ok
}

// statRollout stats a logical rollout path, falling back to its compressed copy.
func statRollout(path string) (os.FileInfo, error) {
	info, err := os.Stat(path)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return info, err
	}
	compressed, zstErr := os.Stat(path + compressedRolloutSuffix)
	if zstErr != nil || compressed.IsDir() {
		return nil, err
	}
	size, sizeErr := compressedContentSize(path+compressedRolloutSuffix, compressed)
	if sizeErr != nil {
		return nil, sizeErr
	}
	return rolloutInfo{FileInfo: compressed, size: size}, nil
}

// openRollout opens a logical rollout path for reading its JSONL content.
func openRollout(path string) (io.ReadCloser, error) {
	file, err := os.Open(path)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return file, err
	}
	compressed, zstErr := os.Open(path + compressedRolloutSuffix)
	if zstErr != nil {
		return nil, err
	}
	decoder, zstErr := zstd.NewReader(compressed, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
	if zstErr != nil {
		compressed.Close()
		return nil, zstErr
	}
	return &zstdRollout{decoder: decoder, file: compressed}, nil
}

// openRolloutAt opens a logical rollout path positioned at offset.
func openRolloutAt(path string, offset int64) (io.ReadCloser, error) {
	reader, err := openRollout(path)
	if err != nil || offset == 0 {
		return reader, err
	}
	if seeker, ok := reader.(io.Seeker); ok {
		_, err = seeker.Seek(offset, io.SeekStart)
	} else {
		_, err = io.CopyN(io.Discard, reader, offset)
	}
	if err != nil {
		reader.Close()
		return nil, err
	}
	return reader, nil
}

type zstdRollout struct {
	decoder *zstd.Decoder
	file    *os.File
}

func (z *zstdRollout) Read(p []byte) (int, error) { return z.decoder.Read(p) }

func (z *zstdRollout) Close() error {
	z.decoder.Close()
	return z.file.Close()
}

type compressedSizeKey struct {
	path          string
	size          int64
	modifiedNanos int64
}

var compressedSizes sync.Map

// compressedContentSize returns the decompressed size of a zstd file. Compressed
// rollouts never change, so the result is cached per file version.
func compressedContentSize(path string, info os.FileInfo) (int64, error) {
	key := compressedSizeKey{pathKey(path), info.Size(), info.ModTime().UnixNano()}
	if cached, ok := compressedSizes.Load(key); ok {
		return cached.(int64), nil
	}
	size, ok, err := frameContentSize(path)
	if err != nil {
		return 0, err
	}
	if !ok {
		size, err = decodedSize(path)
		if err != nil {
			return 0, err
		}
	}
	compressedSizes.Store(key, size)
	return size, nil
}

// frameContentSize sums the declared content size of every frame by hopping
// between block headers, without decompressing. ok is false when a frame omits
// its size or the layout is not understood.
func frameContentSize(path string) (total int64, ok bool, err error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, false, err
	}
	end := info.Size()
	head := make([]byte, zstd.HeaderMaxSize)
	for offset := int64(0); offset < end; {
		n, readErr := file.ReadAt(head, offset)
		if n == 0 || (readErr != nil && !errors.Is(readErr, io.EOF)) {
			return 0, false, nil
		}
		var header zstd.Header
		if header.Decode(head[:n]) != nil {
			return 0, false, nil
		}
		if header.Skippable {
			offset += int64(header.HeaderSize) + int64(header.SkippableSize)
			continue
		}
		if !header.HasFCS || header.FrameContentSize > uint64(1<<62) {
			return 0, false, nil
		}
		total += int64(header.FrameContentSize)
		offset += int64(header.HeaderSize)
		for last := false; !last; {
			var block [3]byte
			if _, err := file.ReadAt(block[:], offset); err != nil {
				return 0, false, nil
			}
			value := uint32(block[0]) | uint32(block[1])<<8 | uint32(block[2])<<16
			length := int64(value >> 3)
			switch (value >> 1) & 3 {
			case 1: // RLE block: one byte repeated length times.
				length = 1
			case 3:
				return 0, false, nil
			}
			offset += 3 + length
			last = value&1 == 1
		}
		if header.HasCheckSum {
			offset += 4
		}
	}
	return total, true, nil
}

func decodedSize(path string) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	decoder, err := zstd.NewReader(file, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
	if err != nil {
		return 0, err
	}
	defer decoder.Close()
	size, err := io.Copy(io.Discard, decoder)
	if err != nil {
		return 0, fmt.Errorf("解壓縮 %s 失敗：%w", path, err)
	}
	return size, nil
}
