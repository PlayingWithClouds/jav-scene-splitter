// Command jav-scene-splitter finds the scene boundaries of a long video, local
// or remote. It reads where the video's bytes thin out (fades compress to
// almost nothing), then decodes only those spots to confirm the transition.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"time"

	"github.com/PlayingWithClouds/jav-scene-splitter/internal/analysis"
	"github.com/PlayingWithClouds/jav-scene-splitter/internal/hls"
	"github.com/PlayingWithClouds/jav-scene-splitter/internal/media"
	"github.com/PlayingWithClouds/jav-scene-splitter/internal/mp4"
	"github.com/PlayingWithClouds/jav-scene-splitter/internal/source"
)

type config struct {
	location string
	headers  http.Header
	output   string
	chapters string
	sizesCSV string
	// probes is how many size dips get decoded.
	probes             int
	maxDipRatio        float64
	blackKeyframeRatio float64
	minScene           time.Duration
	edgeMargin         time.Duration
	workers            int
	quiet              bool
	// truth holds known boundary times (seconds) to score the result against.
	truth []float64
}

func main() {
	settings, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	err = run(ctx, settings)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// headerFlag collects repeated -H "Name: value" flags.
type headerFlag struct{ headers http.Header }

func (flagValue *headerFlag) String() string { return "" }

// Set parses one "Name: value" header.
func (flagValue *headerFlag) Set(raw string) error {
	name, value, found := strings.Cut(raw, ":")
	if !found {
		return fmt.Errorf("header %q must look like 'Name: value'", raw)
	}
	flagValue.headers.Add(strings.TrimSpace(name), strings.TrimSpace(value))
	return nil
}

// parseFlags reads the command line into a config.
func parseFlags(arguments []string) (config, error) {
	settings := config{}
	headers := &headerFlag{headers: http.Header{}}
	flags := flag.NewFlagSet("jav-scene-splitter", flag.ContinueOnError)
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "usage: jav-scene-splitter [flags] <file|url>")
		flags.PrintDefaults()
	}
	flags.Var(headers, "H", "extra HTTP header 'Name: value' (repeatable)")
	flags.StringVar(&settings.output, "o", "", "write scenes JSON here instead of stdout")
	flags.StringVar(&settings.chapters, "chapters", "", "also write ffmetadata chapters to this file")
	flags.StringVar(&settings.sizesCSV, "sizes-csv", "", "only dump the size profile to this CSV and exit (for tuning)")
	flags.IntVar(&settings.probes, "probes", 60, "how many of the deepest size dips to decode")
	flags.Float64Var(&settings.maxDipRatio, "max-dip", 0.8, "ignore dips shallower than this fraction of their baseline")
	flags.Float64Var(&settings.blackKeyframeRatio, "black-keyframe", 0.1, "a keyframe under this fraction of the median keyframe size counts as black")
	flags.DurationVar(&settings.minScene, "min-scene", 20*time.Second, "drop a transition this close to the previous one")
	flags.DurationVar(&settings.edgeMargin, "edge", 15*time.Second, "ignore transitions this close to the start or end")
	flags.IntVar(&settings.workers, "workers", min(6, runtime.NumCPU()), "parallel requests and decodes")
	flags.BoolVar(&settings.quiet, "q", false, "no progress output")
	truth := flags.String("truth", "", "known boundaries to score against, e.g. 57:40,1:19:47")
	err := flags.Parse(arguments)
	if err != nil {
		return settings, err
	}
	settings.truth, err = parseTimestamps(*truth)
	if err != nil {
		return settings, err
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return settings, fmt.Errorf("expected exactly one file or URL")
	}
	settings.location = flags.Arg(0)
	settings.headers = headers.headers
	return settings, nil
}

// run reads the size profile, picks candidate windows, decodes them to
// confirm transitions, and writes the scenes.
func run(ctx context.Context, settings config) error {
	started := time.Now()
	video, err := openVideo(ctx, settings.location, source.NewClient(settings.headers))
	if err != nil {
		return err
	}
	defer video.Close()
	logf(settings, "%s, %s", video.Describe(), formatTimestamp(video.Duration()))

	profile, err := video.SizeProfile(ctx, settings.workers)
	if err != nil {
		return err
	}
	logf(settings, "size profile: %d bins, %s fetched", len(profile), megabytes(video.BytesFetched()))
	if settings.sizesCSV != "" {
		return writeSizesCSV(settings.sizesCSV, profile)
	}

	windows := analysis.FindCandidates(profile, analysis.CandidateOptions{
		BlackKeyframeRatio: settings.blackKeyframeRatio,
		Probes:             settings.probes,
		MaxDipRatio:        settings.maxDipRatio,
	})
	confirmed := analysis.NewInspector(video).InspectAll(ctx, windows, settings.workers)
	logf(settings, "decoded %d candidate windows, %d transitions, %s fetched in total",
		len(windows), len(confirmed), megabytes(video.BytesFetched()))
	boundaries := analysis.SelectBoundaries(confirmed, video.Duration(), analysis.SelectOptions{
		EdgeMargin: settings.edgeMargin.Seconds(),
		MinScene:   settings.minScene.Seconds(),
	})

	result := buildResult(settings.location, video, boundaries, time.Since(started))
	err = writeResult(settings.output, result)
	if err != nil {
		return err
	}
	if len(settings.truth) > 0 {
		reportAgainstTruth(boundaries, settings.truth, windows)
	}
	if settings.chapters != "" {
		return writeChapters(settings.chapters, result)
	}
	return nil
}

// openVideo picks the container reader: HLS for playlists, MP4 otherwise.
func openVideo(ctx context.Context, location string, client *source.Client) (media.Video, error) {
	if source.IsURL(location) && looksLikePlaylist(location) {
		body, err := client.Get(ctx, location)
		if err != nil {
			return nil, err
		}
		if !hls.IsPlaylist(body) {
			return nil, fmt.Errorf("%s is not an M3U8 playlist", location)
		}
		return hls.Open(ctx, client, location, body)
	}
	bytes, err := source.Open(ctx, location, client)
	if err != nil {
		return nil, err
	}
	return mp4.Open(ctx, bytes)
}

// looksLikePlaylist checks the URL path for an .m3u8 extension.
func looksLikePlaylist(location string) bool {
	path, _, _ := strings.Cut(location, "?")
	return strings.HasSuffix(strings.ToLower(path), ".m3u8")
}

// megabytes formats a byte count for logs.
func megabytes(count int64) string {
	return fmt.Sprintf("%.1f MB", float64(count)/1e6)
}

// logf prints a progress line to stderr unless -q.
func logf(settings config, format string, arguments ...any) {
	if settings.quiet {
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", arguments...)
}
