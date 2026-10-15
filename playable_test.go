package main

import (
	"archive/zip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func needFFmpeg(t *testing.T) {
	t.Helper()
	if !haveFFmpeg() {
		t.Skip("ffmpeg not installed")
	}
}

// makeVideo writes a 2-second clip with the given audio codec and container.
func makeVideo(t *testing.T, out, audioCodec string, withSubs bool) {
	t.Helper()
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=160x120:rate=10:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2"}
	if withSubs {
		srt := filepath.Join(t.TempDir(), "s.srt")
		os.WriteFile(srt, []byte("1\n00:00:00,000 --> 00:00:01,500\nhello\n"), 0o644)
		args = append(args, "-i", srt, "-map", "0", "-map", "1", "-map", "2", "-c:s", "srt")
	}
	args = append(args, "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", audioCodec, "-ac", "6", "-shortest", out)
	if msg, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Skipf("can't make test video (%v): %s", err, msg)
	}
}

func codecs(t *testing.T, file string) (format string, video, audio, subs []string) {
	t.Helper()
	p, err := ffprobe(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range p.Streams {
		switch s.CodecType {
		case "video":
			video = append(video, s.CodecName)
		case "audio":
			audio = append(audio, s.CodecName)
		case "subtitle":
			subs = append(subs, s.CodecName)
		}
	}
	return p.Format.FormatName, video, audio, subs
}

func TestMakePlayableVideo(t *testing.T) {
	needFFmpeg(t)
	dir := t.TempDir()
	mkv := filepath.Join(dir, "Episode 1.mkv")
	makeVideo(t, mkv, "eac3", true)

	out, note, err := makePlayable(context.Background(), mkv)
	if err != nil {
		t.Fatal(err)
	}
	if out != filepath.Join(dir, "Episode 1.mp4") || !strings.Contains(note, "AAC") {
		t.Fatalf("got %s (%s)", out, note)
	}
	if _, err := os.Stat(mkv); !os.IsNotExist(err) {
		t.Fatal("original mkv should be replaced")
	}
	format, v, a, s := codecs(t, out)
	if !strings.Contains(format, "mp4") || v[0] != "h264" || a[0] != "aac" || len(s) != 1 || s[0] != "mov_text" {
		t.Fatalf("got format=%s video=%v audio=%v subs=%v", format, v, a, s)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".*")); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}

	// An MP4 that's already fine is left alone.
	good := filepath.Join(dir, "good.mp4")
	makeVideo(t, good, "aac", false)
	before, _ := os.Stat(good)
	out, note, err = makePlayable(context.Background(), good)
	after, _ := os.Stat(good)
	if err != nil || out != good || !after.ModTime().Equal(before.ModTime()) || !strings.Contains(note, "already") {
		t.Fatalf("good mp4 touched: %s %s %v", out, note, err)
	}
}

func TestMakePlayableZip(t *testing.T) {
	needFFmpeg(t)
	dir := t.TempDir()
	mkv := filepath.Join(dir, "a.mkv")
	makeVideo(t, mkv, "ac3", false)
	good := filepath.Join(dir, "b.mp4")
	makeVideo(t, good, "aac", false)
	mkvData, _ := os.ReadFile(mkv)
	goodData, _ := os.ReadFile(good)

	zipPath := filepath.Join(dir, "show.zip")
	writeZip(t, zipPath, map[string][]byte{
		"s1/ep1.mkv": mkvData,
		"s1/ep2.mp4": goodData,
		"notes.txt":  []byte("keep me"),
	}, zip.Deflate)

	out, note, err := makePlayable(context.Background(), zipPath)
	if err != nil || out != zipPath || !strings.Contains(note, "1 video") {
		t.Fatalf("got %s %q %v", out, note, err)
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	names := map[string]*zip.File{}
	for _, f := range zr.File {
		names[f.Name] = f
	}
	if names["s1/ep1.mkv"] != nil || names["s1/ep1.mp4"] == nil || names["s1/ep2.mp4"] == nil || names["notes.txt"] == nil {
		t.Fatalf("unexpected entries: %v", names)
	}
	if names["s1/ep1.mp4"].Method != zip.Store {
		t.Fatal("converted video should be stored uncompressed for seeking")
	}
	if !bytesOf(t, names["s1/ep2.mp4"], goodData) || !bytesOf(t, names["notes.txt"], []byte("keep me")) {
		t.Fatal("untouched entries changed")
	}
	tmp := filepath.Join(t.TempDir(), "ep1.mp4")
	extract(names["s1/ep1.mp4"], tmp)
	if _, _, a, _ := codecs(t, tmp); a[0] != "aac" {
		t.Fatalf("audio in zip is %v", a)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".*")); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}

func bytesOf(t *testing.T, zf *zip.File, want []byte) bool {
	t.Helper()
	rc, err := zf.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got := make([]byte, len(want)+1)
	n, _ := rc.Read(got)
	for n < len(got) {
		m, err := rc.Read(got[n:])
		n += m
		if err != nil {
			break
		}
	}
	return string(got[:n]) == string(want)
}
