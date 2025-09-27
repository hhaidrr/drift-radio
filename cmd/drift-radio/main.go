package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creativeprojects/go-selfupdate"
)

type Station struct {
	Name        string
	URL         string
	Description string
}

var defaultStations = []Station{
	{Name: "Lofi Hip Hop Radio - beats to relax/study to", URL: "https://www.youtube.com/watch?v=jfKfPfyJRdk", Description: "The most popular lofi radio station"},
	{Name: "ChilledCow - Lofi Hip Hop Radio", URL: "https://www.youtube.com/watch?v=5qap5aO4i9A", Description: "Classic lofi beats for studying"},
	{Name: "Lofi Girl - 24/7 lofi hip hop radio", URL: "https://www.youtube.com/watch?v=DWcJFNfaw9c", Description: "24/7 lofi hip hop radio stream"},
	{Name: "Chillhop Music - Lofi Hip Hop Radio", URL: "https://www.youtube.com/watch?v=7NOSDKb0HlU", Description: "Chillhop lofi radio"},
	{Name: "Lofi Hip Hop Radio - Beats to sleep/chill to", URL: "https://www.youtube.com/watch?v=rUxyKA_-grg", Description: "Relaxing lofi beats for sleep"},
}

type Player struct {
	mu             sync.Mutex
	cmd            *exec.Cmd
	currentStation int
	volumePercent  int
	isStopped      bool
	visualization  bool
	analyzer       *StreamAnalyzer
}

func NewPlayer() *Player {
	return &Player{
		currentStation: 0,
		volumePercent:  70,
		visualization:  false,
		analyzer:       NewStreamAnalyzer(),
	}
}

func (p *Player) ffplayArgs(url string) []string {
	// ffplay volume uses dB via -af volume=...; map 0-100% to -20..+0 dB approx
	volDb := float64(p.volumePercent)/100*0 - 20*(1-float64(p.volumePercent)/100)
	volFilter := fmt.Sprintf("volume=%fdB", volDb)
	args := []string{
		"-nodisp",
		"-autoexit",
		"-loglevel", "warning", // Keep warning level for audio processing
		"-hide_banner", // Hide ffplay banner
		"-af", volFilter,
		url,
	}
	if p.visualization {
		// Use showwavespic as a lightweight visualization in a separate window
		// However -nodisp disables it; keep nodisp for headless. Toggle simply prints a note.
	}
	return args
}

func (p *Player) Start(url string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd != nil && p.cmd.Process != nil {
		return errors.New("player already running")
	}
	resolved, err := resolvePlayableURL(url)
	if err != nil {
		return err
	}

	// Start stream analysis
	if err := p.analyzer.StartAnalysis(resolved); err != nil {
		// Don't fail the entire start if analysis fails
		fmt.Printf("Warning: Could not start stream analysis: %v\n", err)
	}

	args := p.ffplayArgs(resolved)
	p.cmd = exec.Command("ffplay", args...)
	p.cmd.Stdout = os.Stdout
	p.cmd.Stderr = os.Stderr
	if err := p.cmd.Start(); err != nil {
		p.cmd = nil
		return err
	}
	p.isStopped = false
	go func(cmd *exec.Cmd) {
		_ = cmd.Wait()
		p.mu.Lock()
		defer p.mu.Unlock()
		p.cmd = nil
	}(p.cmd)
	return nil
}

func (p *Player) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd == nil || p.cmd.Process == nil {
		p.isStopped = true
		p.analyzer.StopAnalysis()
		return nil
	}
	p.isStopped = true

	// Stop stream analysis
	p.analyzer.StopAnalysis()

	// Send SIGTERM to stop the process
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return err
	}

	// Wait for the process to actually exit
	done := make(chan error, 1)
	go func() {
		done <- p.cmd.Wait()
	}()

	select {
	case <-done:
		// Process exited
		p.cmd = nil
		return nil
	case <-time.After(2 * time.Second):
		// Force kill if it doesn't exit within 2 seconds
		p.cmd.Process.Kill()
		p.cmd = nil
		return nil
	}
}

func (p *Player) Restart(url string) error {
	_ = p.Stop()
	return p.Start(url)
}

// resolvePlayableURL returns a direct media URL that ffplay can consume.
// For YouTube links, it uses yt-dlp -g to get the direct audio URL (same as your working command).
func resolvePlayableURL(originalURL string) (string, error) {
	if !isYouTubeURL(originalURL) {
		return originalURL, nil
	}

	// Use yt-dlp -g to get the direct audio URL (same as your working command)
	cmd := exec.Command(ytdlpBinary, "-g", "-f", "bestaudio/best", originalURL)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("yt-dlp failed: %v, stderr: %s", err, stderr.String())
	}

	output := strings.TrimSpace(stdout.String())
	if output == "" {
		return "", fmt.Errorf("yt-dlp did not return a media URL, stderr: %s", stderr.String())
	}

	// Return the first line (the audio URL)
	lines := strings.Split(output, "\n")
	return strings.TrimSpace(lines[0]), nil
}

var ytRegexp = regexp.MustCompile(`(?i)^(https?://)?(www\.)?(youtube\.com|youtu\.be)/`)

func isYouTubeURL(u string) bool {
	return ytRegexp.MatchString(u)
}

// checkDependencies verifies that required external tools are available
func checkDependencies() error {
	// Check for ffplay
	if _, err := exec.LookPath("ffplay"); err != nil {
		return fmt.Errorf("ffplay not found. Please install FFmpeg: sudo apt install ffmpeg")
	}

	// Check for yt-dlp (needed for YouTube URLs) - use system yt-dlp
	if _, err := exec.LookPath("yt-dlp"); err != nil {
		return fmt.Errorf("yt-dlp not found. Please install yt-dlp: sudo curl -L https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp -o /usr/local/bin/yt-dlp && sudo chmod a+rx /usr/local/bin/yt-dlp")
	}

	// Use system yt-dlp
	ytdlpBinary = "yt-dlp"

	return nil
}

var ytdlpBinary string

func (p *Player) SetVolume(percent int) {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	p.volumePercent = percent
}

func (p *Player) showQualityAlerts() {
	if p.analyzer == nil {
		return
	}

	alerts := p.analyzer.GetQualityAlerts()

	if len(alerts) > 0 {
		fmt.Println("\n⚠️  Quality Alerts:")
		for _, alert := range alerts {
			fmt.Printf("   • %s\n", alert)
		}
	}
}

func (p *Player) displayStatsLoop(ctx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !p.isStopped {
				// Clear screen and show stats
				fmt.Print("\033[2J\033[H") // Clear screen and move cursor to top
				fmt.Print(p.analyzer.FormatStats())

				// Show quality alerts
				alerts := p.analyzer.GetQualityAlerts()
				if len(alerts) > 0 {
					fmt.Println("\n⚠️  Quality Alerts:")
					for _, alert := range alerts {
						fmt.Printf("   • %s\n", alert)
					}
				}

				fmt.Print("radio> ")
			}
		}
	}
}

func printHeader(volume int, nowPlaying string) {
	fmt.Printf("\n\U0001F50A Volume set to %d%%\n", volume)
	fmt.Printf("\U0001F3B5 Now Playing: %s\n", nowPlaying)
	fmt.Println("\u23F3 Loading stream...")
}

func printHelp() {
	fmt.Println()
	fmt.Println("\U0001F4AA Controls:")
	fmt.Println("  [s] Stop playback")
	fmt.Println("  [v] Change volume")
	fmt.Println("  [l] List all stations")
	fmt.Println("  [viz] Toggle visualization")
	fmt.Println("  [q] Quit")
	fmt.Println("  [h] Show this help")
	fmt.Println("  [1-5] Switch station")
	fmt.Println()
	fmt.Println("📊 Stream quality stats are displayed automatically")
	fmt.Println()
}

func listStations(stations []Station) {
	fmt.Println("Available Stations:")
	for i, s := range stations {
		fmt.Printf("  [%d] %s\n", i+1, s.Name)
		fmt.Printf("      %s\n", s.Description)
	}
}

func interactiveMode(ctx context.Context, p *Player, stations []Station, startIdx int) {
	if startIdx < 0 || startIdx >= len(stations) {
		startIdx = 0
	}
	p.currentStation = startIdx
	now := stations[p.currentStation]
	printHeader(p.volumePercent, now.Name)
	_ = p.Start(now.URL)
	printHelp()

	// Start real-time stats display immediately
	statsCtx, statsCancel := context.WithCancel(ctx)
	defer statsCancel()
	go p.displayStatsLoop(statsCtx)

	reader := bufio.NewReader(os.Stdin)
	fmt.Print("radio> ")
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			fmt.Println("input error:", err)
			return
		}
		input := strings.TrimSpace(line)
		switch input {
		case "q":
			_ = p.Stop()
			return
		case "h":
			printHelp()
		case "s":
			_ = p.Stop()
		case "v":
			fmt.Print("Enter volume (0-100): ")
			vline, _ := reader.ReadString('\n')
			vline = strings.TrimSpace(vline)
			var v int
			fmt.Sscanf(vline, "%d", &v)
			p.SetVolume(v)
			fmt.Printf("Volume set to %d%%\n", p.volumePercent)
			// restart if currently playing
			if !p.isStopped {
				_ = p.Restart(stations[p.currentStation].URL)
			}
		case "l":
			listStations(stations)
		case "viz":
			p.visualization = !p.visualization
			state := "OFF"
			if p.visualization {
				state = "ON"
			}
			fmt.Println("Visualization:", state)
		case "1", "2", "3", "4", "5":
			idx := int(input[0] - '1')
			if idx >= 0 && idx < len(stations) {
				p.currentStation = idx
				now = stations[p.currentStation]
				fmt.Println("Switching to:", now.Name)
				if err := p.Restart(now.URL); err != nil {
					fmt.Printf("Failed to start station: %v\n", err)
				} else {
					fmt.Println("✓ Now playing:", now.Name)
				}
			} else {
				fmt.Println("Invalid station number")
			}
		default:
			if input != "" {
				fmt.Println("Unknown command. Press 'h' for help.")
			}
		}
		fmt.Print("radio> ")
	}
}

func main() {
	// Check dependencies first
	if err := checkDependencies(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	var (
		flagList        bool
		flagInteractive bool
		flagStation     int
		flagVolume      int
	)
	flag.BoolVar(&flagInteractive, "i", true, "interactive mode")
	flag.BoolVar(&flagList, "list", false, "list stations and exit")
	flag.IntVar(&flagStation, "station", 1, "station number to start (1-5)")
	flag.IntVar(&flagVolume, "volume", 70, "start volume 0-100")
	flag.Parse()

	p := NewPlayer()
	p.SetVolume(flagVolume)

	if flagList {
		listStations(defaultStations)
		return
	}

	startIdx := flagStation - 1
	if startIdx < 0 || startIdx >= len(defaultStations) {
		startIdx = 0
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		_ = p.Stop()
		cancel()
	}()

	if flagInteractive {
		interactiveMode(ctx, p, defaultStations, startIdx)
		return
	}

	// Standard mode: start and wait until Ctrl+C
	st := defaultStations[startIdx]
	printHeader(p.volumePercent, st.Name)
	if err := p.Start(st.URL); err != nil {
		fmt.Println("Failed to start:", err)
		os.Exit(1)
	}
	printHelp()

	// Start real-time stats display
	statsCtx, statsCancel := context.WithCancel(ctx)
	defer statsCancel()
	go p.displayStatsLoop(statsCtx)

	<-ctx.Done()
}
