package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Making videos browser-playable without re-encoding video.
//
// Browsers reliably play H.264/HEVC/VP9/AV1 video with AAC, MP3 or Opus audio
// in MP4. Downloads often arrive as MKV with E-AC-3, AC-3, DTS or TrueHD audio,
// which most browsers can't play. The fix copies the video stream as-is,
// converts only the audio to AAC, and rewrites the container as MP4, which
// takes seconds to minutes rather than hours. Text subtitles are kept as
// mov_text; image subtitles (PGS/VobSub) can't go into MP4 and are dropped.

var videoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".mov": true, ".avi": true,
	".webm": true, ".ts": true, ".m2ts": true, ".wmv": true, ".flv": true,
}

func isVideoName(name string) bool { return videoExts[strings.ToLower(path.Ext(name))] }

var (
	playableAudio = map[string]bool{"aac": true, "mp3": true, "opus": true}
	textSubs      = map[string]bool{"subrip": true, "ass": true, "ssa": true, "mov_text": true, "webvtt": true, "text": true}
)

const convertTimeout = 3 * time.Hour

type probeResult struct {
	Streams []struct {
		Index       int    `json:"index"`
		CodecType   string `json:"codec_type"`
		CodecName   string `json:"codec_name"`
		Disposition struct {
			AttachedPic int `json:"attached_pic"`
		} `json:"disposition"`
	} `json:"streams"`
	Format struct {
		FormatName string `json:"format_name"`
	} `json:"format"`
}

func ffprobe(ctx context.Context, file string) (*probeResult, error) {
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-print_format", "json",
		"-show_streams", "-show_format", file).Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}
	var p probeResult
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, fmt.Errorf("ffprobe output: %w", err)
	}
	return &p, nil
}

// conversionPlan says whether a file needs fixing, builds the ffmpeg
// arguments (input and output are appended by the caller) and describes the
// change for the fetch page.
func conversionPlan(p *probeResult, name string) (args []string, desc string, needed bool) {
	video := -1
	videoCodec := ""
	var audio, subs []int
	audioOK := true
	for _, s := range p.Streams {
		switch s.CodecType {
		case "video":
			if video < 0 && s.Disposition.AttachedPic == 0 {
				video, videoCodec = s.Index, s.CodecName
			}
		case "audio":
			audio = append(audio, s.Index)
			if !playableAudio[s.CodecName] {
				audioOK = false
			}
		case "subtitle":
			if textSubs[s.CodecName] {
				subs = append(subs, s.Index)
			}
		}
	}
	if video < 0 {
		return nil, "", false
	}
	isMP4 := strings.Contains(p.Format.FormatName, "mp4") &&
		map[string]bool{".mp4": true, ".m4v": true}[strings.ToLower(path.Ext(name))]
	if audioOK && isMP4 {
		return nil, "", false
	}
	var changes []string
	if !audioOK {
		changes = append(changes, "audio converted to AAC")
	}
	if !isMP4 {
		changes = append(changes, strings.ToUpper(strings.TrimPrefix(path.Ext(name), "."))+" → MP4")
	}
	desc = strings.Join(changes, ", ")

	args = []string{"-map", fmt.Sprintf("0:%d", video)}
	for _, a := range audio {
		args = append(args, "-map", fmt.Sprintf("0:%d", a))
	}
	for _, s := range subs {
		args = append(args, "-map", fmt.Sprintf("0:%d", s))
	}
	args = append(args, "-map_metadata", "0", "-c:v", "copy")
	if videoCodec == "hevc" {
		args = append(args, "-tag:v", "hvc1") // needed for Safari
	}
	if audioOK {
		args = append(args, "-c:a", "copy")
	} else {
		// Stereo AAC plays everywhere; 5.1 AAC fails in some browsers.
		args = append(args, "-c:a", "aac", "-b:a", "192k", "-ac", "2")
	}
	if len(subs) > 0 {
		args = append(args, "-c:s", "mov_text")
	}
	args = append(args, "-movflags", "+faststart", "-f", "mp4")
	return args, desc, true
}

// convertFile writes a browser-playable MP4 of in to out, if in needs it,
// and says what changed.
func convertFile(ctx context.Context, in, out, name string) (bool, string, error) {
	p, err := ffprobe(ctx, in)
	if err != nil {
		return false, "", err
	}
	args, desc, needed := conversionPlan(p, name)
	if !needed {
		return false, "", nil
	}
	full := append([]string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-i", in}, args...)
	full = append(full, out)
	cmd := exec.CommandContext(ctx, "ffmpeg", full...)
	if msg, err := cmd.CombinedOutput(); err != nil {
		os.Remove(out)
		return false, "", fmt.Errorf("ffmpeg: %v: %s", err, strings.TrimSpace(lastLine(string(msg))))
	}
	return true, desc, nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func mp4Name(name string) string { return strings.TrimSuffix(name, path.Ext(name)) + ".mp4" }

// makePlayable fixes a downloaded file in place. For a video it returns the
// new path (an .mp4 next to it, replacing the original); for a zip, the zip
// keeps its path and only the videos inside change. note describes what
// happened, for the fetch page.
func makePlayable(ctx context.Context, file string) (newPath, note string, err error) {
	ctx, cancel := context.WithTimeout(ctx, convertTimeout)
	defer cancel()
	switch {
	case isZip(file):
		n, err := fixZip(ctx, file)
		if err != nil {
			return file, "", err
		}
		if n == 0 {
			return file, "videos already browser-friendly", nil
		}
		return file, fmt.Sprintf("made %d video(s) in the zip browser-playable", n), nil
	case isVideoName(file):
		dir, name := filepath.Split(file)
		tmp := filepath.Join(dir, "."+mp4Name(name)+".convert.part")
		changed, desc, err := convertFile(ctx, file, tmp, name)
		if err != nil || !changed {
			if err == nil {
				note = "already browser-friendly"
			}
			return file, note, err
		}
		dest := filepath.Join(dir, mp4Name(name))
		if dest == file {
			err = os.Rename(tmp, dest) // replacing the original is the point
		} else {
			dest, err = moveToFreeName(tmp, dir, mp4Name(name))
		}
		if err != nil {
			os.Remove(tmp)
			return file, "", err
		}
		if dest != file {
			os.Remove(file)
		}
		return dest, desc, nil
	}
	return file, "", nil
}

// fixZip converts the videos inside a zip that need it and rewrites the zip at
// the same path: other entries are copied byte-for-byte, fixed videos are
// stored uncompressed (so they seek instantly) under an .mp4 name. Returns how
// many videos were changed.
func fixZip(ctx context.Context, zipPath string) (int, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return 0, err
	}
	defer zr.Close()

	dir := filepath.Dir(zipPath)
	work, err := os.MkdirTemp(dir, ".convert-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(work)

	taken := map[string]bool{}
	for _, zf := range zr.File {
		taken[zf.Name] = true
	}
	type fixed struct{ name, file string }
	replace := map[*zip.File]fixed{}
	for i, zf := range zr.File {
		if zf.FileInfo().IsDir() || !isVideoName(zf.Name) {
			continue
		}
		in := filepath.Join(work, fmt.Sprintf("in-%d%s", i, path.Ext(zf.Name)))
		if err := extract(zf, in); err != nil {
			return 0, fmt.Errorf("%s: %w", zf.Name, err)
		}
		out := filepath.Join(work, fmt.Sprintf("out-%d.mp4", i))
		changed, _, err := convertFile(ctx, in, out, zf.Name)
		os.Remove(in)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", zf.Name, err)
		}
		if !changed {
			continue
		}
		name := mp4Name(zf.Name)
		if name != zf.Name {
			stem := strings.TrimSuffix(name, ".mp4")
			for n := 2; taken[name]; n++ {
				name = fmt.Sprintf("%s (%d).mp4", stem, n)
			}
			taken[name] = true
		}
		replace[zf] = fixed{name, out}
	}
	if len(replace) == 0 {
		return 0, nil
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(zipPath)+".*.part")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	zw := zip.NewWriter(tmp)
	for _, zf := range zr.File {
		fx, ok := replace[zf]
		if !ok {
			if err := zw.Copy(zf); err != nil {
				tmp.Close()
				return 0, err
			}
			continue
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: fx.name, Method: zip.Store, Modified: zf.Modified})
		if err == nil {
			err = copyFileTo(w, fx.file)
		}
		if err != nil {
			tmp.Close()
			return 0, err
		}
	}
	if err := zw.Close(); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	os.Chmod(tmp.Name(), 0o644)
	if err := os.Rename(tmp.Name(), zipPath); err != nil {
		return 0, err
	}
	return len(replace), nil
}

func extract(zf *zip.File, dest string) error {
	rc, err := zf.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func copyFileTo(w io.Writer, file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

var errNoFFmpeg = errors.New("ffmpeg is not installed on the server")

func haveFFmpeg() bool {
	_, e1 := exec.LookPath("ffmpeg")
	_, e2 := exec.LookPath("ffprobe")
	return e1 == nil && e2 == nil
}
