package butterfish

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/creack/pty"
	"github.com/mitchellh/go-homedir"
	"golang.org/x/term"

	"github.com/bakks/butterfish/bubbles/console"
	"github.com/bakks/butterfish/embedding"
	"github.com/bakks/butterfish/prompt"
	"github.com/bakks/butterfish/util"
)

// Main driver for the Butterfish set of command line tools. These are tools
// for using AI capabilities on the command line.

type ButterfishConfig struct {
	Verbose     bool
	OpenAIToken string
	LLMClient   LLM
	ColorScheme *ColorScheme
	Styles      *styles

	PromptLibraryPath string
	PromptLibrary     PromptLibrary
}

type PromptLibrary interface {
	GetPrompt(name string, args ...string) (string, error)
}

type LLM interface {
	CompletionStream(request *util.CompletionRequest, writer io.Writer) (string, error)
	Completion(request *util.CompletionRequest) (string, error)
	Embeddings(ctx context.Context, input []string) ([][]float64, error)
	Edits(ctx context.Context, content, instruction, model string, temperature float32) (string, error)
}

type ButterfishCtx struct {
	Ctx    context.Context    // global context, should be passed through to other calls
	Cancel context.CancelFunc // cancel function for the global context
	Out    io.Writer          // output writer

	Config          *ButterfishConfig            // configuration
	InConsoleMode   bool                         // true if we're running in console mode
	PromptLibrary   PromptLibrary                // library of prompts
	LLMClient       LLM                          // GPT client
	CommandRegister string                       // landing space for generated commands
	VectorIndex     embedding.FileEmbeddingIndex // embedding index for searching local files

	ConsoleCmdChan   <-chan string    // channel for console commands
	ClientController ClientController // client controller
}

type ColorScheme struct {
	Foreground string // neutral foreground color
	Background string
	Error      string // should be reddish
	Color1     string // should be greenish
	Color2     string
	Color3     string
	Color4     string
	Color5     string
	Color6     string
	Grey       string
}

// Gruvbox Colorscheme
// from https://github.com/morhetz/gruvbox
var GruvboxDark = ColorScheme{
	Foreground: "#ebdbb2",
	Background: "#282828",
	Error:      "#fb4934", // red
	Color1:     "#b8bb26", // green
	Color2:     "#fabd2f", // yellow
	Color3:     "#83a598", // blue
	Color4:     "#d3869b", // magenta
	Color5:     "#8ec07c", // cyan
	Color6:     "#fe8019", // orange
	Grey:       "#928374", // gray
}

var GruvboxLight = ColorScheme{
	Foreground: "#7C6F64",
	Background: "#FBF1C7",
	Error:      "#CC241D",
	Color1:     "#98971A",
	Color2:     "#D79921",
	Color3:     "#458588",
	Color4:     "#B16286",
	Color5:     "#689D6A",
	Color6:     "#D65D0E",
	Grey:       "#928374",
}

// Data type for passing byte chunks from a wrapped command around
type byteMsg struct {
	Data []byte
}

func NewByteMsg(data []byte) *byteMsg {
	buf := make([]byte, len(data))
	copy(buf, data)
	return &byteMsg{
		Data: buf,
	}
}

// Given an io.Reader we write byte chunks to a channel
func readerToChannel(input io.Reader, c chan<- *byteMsg) {
	buf := make([]byte, 1024*16)

	// Loop indefinitely
	for {
		// Read from stream
		n, err := input.Read(buf)

		// Check for error
		if err != nil {
			if err != io.EOF {
				log.Printf("Error reading from file: %s\n", err)
			}
			break
		}

		c <- NewByteMsg(buf[:n])
	}

	// Close the channel
	close(c)
}

// from https://github.com/acarl005/stripansi/blob/master/stripansi.go
const ansiPattern = "[\u001B\u009B][[\\]()#;?]*(?:(?:(?:[a-zA-Z\\d]*(?:;[a-zA-Z\\d]*)*)?\u0007)|(?:(?:\\d{1,4}(?:;\\d{0,4})*)?[\\dA-PRZcf-ntqry=><~]))"

var ansiRegexp = regexp.MustCompile(ansiPattern)

// Strip ANSI tty control codes out of a string
func stripANSI(str string) string {
	return ansiRegexp.ReplaceAllString(str, "")
}

// Function for filtering out non-printable characters from a string
func filterNonPrintable(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\t': // we don't want to filter these out though
			return r

		default:
			if unicode.IsPrint(r) {
				return r
			}
			return -1
		}
	}, s)
}

func sanitizeTTYData(data []byte) []byte {
	return []byte(filterNonPrintable(stripANSI(string(data))))
}

// Based on example at https://github.com/creack/pty
// Apparently you can't start a shell like zsh without
// this more complex command execution
func wrapCommand(ctx context.Context, cancel context.CancelFunc, command []string, client *IPCClient) error {
	// Create arbitrary command.
	c := exec.Command(command[0], command[1:]...)

	// Start the command with a pty.
	ptmx, err := pty.Start(c)
	if err != nil {
		return err
	}
	// Make sure to close the pty at the end.
	defer func() { _ = ptmx.Close() }() // Best effort.

	// Handle pty size.
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		for range ch {
			if err := pty.InheritSize(os.Stdin, ptmx); err != nil {
				log.Printf("error resizing pty: %s", err)
			}
		}
	}()
	ch <- syscall.SIGWINCH                        // Initial resize.
	defer func() { signal.Stop(ch); close(ch) }() // Cleanup signals when done.

	// Set stdin in raw mode.
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		panic(err)
	}
	defer func() { _ = term.Restore(int(os.Stdin.Fd()), oldState) }() // Best effort.

	parentIn := make(chan *byteMsg)
	childOut := make(chan *byteMsg)
	remoteIn := make(chan *byteMsg)

	// Read from this process Stdin and write to stdinChannel
	go readerToChannel(os.Stdin, parentIn)
	// Read from pty Stdout and write to stdoutChannel
	go readerToChannel(ptmx, childOut)
	// Read from remote
	go packageRPCStream(client, remoteIn)

	client.SendWrappedCommand(strings.Join(command, " "))

	wrappingMultiplexer(ctx, cancel, client, ptmx, parentIn, remoteIn, childOut)

	return nil
}

func (this *ButterfishCtx) CalculateEmbeddings(ctx context.Context, content []string) ([][]float64, error) {
	return this.LLMClient.Embeddings(ctx, content)
}

// A local printf that writes to the butterfishctx out using a lipgloss style
func (this *ButterfishCtx) StylePrintf(style lipgloss.Style, format string, a ...any) {
	str := util.MultilineLipglossRender(style, fmt.Sprintf(format, a...))
	this.Out.Write([]byte(str))
}

func (this *ButterfishCtx) StyleSprintf(style lipgloss.Style, format string, a ...any) string {
	return util.MultilineLipglossRender(style, fmt.Sprintf(format, a...))
}

func (this *ButterfishCtx) Printf(format string, a ...any) {
	this.StylePrintf(this.Config.Styles.Foreground, format, a...)
}

func (this *ButterfishCtx) ErrorPrintf(format string, a ...any) {
	this.StylePrintf(this.Config.Styles.Error, format, a...)
}

// Ensure we have a vector index object, idempotent
func (this *ButterfishCtx) initVectorIndex(pathsToLoad []string) error {
	if this.VectorIndex != nil {
		return nil
	}

	out := util.NewStyledWriter(this.Out, this.Config.Styles.Foreground)
	index := embedding.NewDiskCachedEmbeddingIndex(this, out)

	if this.Config.Verbose {
		index.SetOutput(this.Out)
	}

	this.VectorIndex = index

	if !this.InConsoleMode {
		// if we're running from the command line then we first load the curr
		// dir index
		if pathsToLoad == nil || len(pathsToLoad) == 0 {
			pathsToLoad = []string{"."}
		}

		err := this.VectorIndex.LoadPaths(this.Ctx, pathsToLoad)
		if err != nil {
			return err
		}
	}

	return nil
}

func (this *ButterfishCtx) printError(err error, prefix ...string) {
	if len(prefix) > 0 {
		fmt.Fprintf(this.Out, "%s error: %s\n", prefix[0], err.Error())
	} else {
		fmt.Fprintf(this.Out, "Error: %s\n", err.Error())
	}
}

type styles struct {
	Question   lipgloss.Style
	Answer     lipgloss.Style
	Go         lipgloss.Style
	Summarize  lipgloss.Style
	Highlight  lipgloss.Style
	Prompt     lipgloss.Style
	Error      lipgloss.Style
	Foreground lipgloss.Style
	Grey       lipgloss.Style
}

func (this *styles) PrintTestColors() {
	fmt.Println("Question: ", this.Question.Render("Question"))
	fmt.Println("Answer: ", this.Answer.Render("Answer"))
	fmt.Println("Go: ", this.Go.Render("Go"))
	fmt.Println("Summarize: ", this.Summarize.Render("Summarize"))
	fmt.Println("Highlight: ", this.Highlight.Render("Highlight"))
	fmt.Println("Prompt: ", this.Prompt.Render("Prompt"))
	fmt.Println("Error: ", this.Error.Render("Error"))
	fmt.Println("Foreground: ", this.Foreground.Render("Foreground"))
	fmt.Println("Grey: ", this.Grey.Render("Grey"))
}

func ColorSchemeToStyles(colorScheme *ColorScheme) *styles {
	return &styles{
		Question:   lipgloss.NewStyle().Foreground(lipgloss.Color(colorScheme.Color4)),
		Answer:     lipgloss.NewStyle().Foreground(lipgloss.Color(colorScheme.Color2)),
		Go:         lipgloss.NewStyle().Foreground(lipgloss.Color(colorScheme.Color5)),
		Highlight:  lipgloss.NewStyle().Foreground(lipgloss.Color(colorScheme.Color2)),
		Summarize:  lipgloss.NewStyle().Foreground(lipgloss.Color(colorScheme.Color2)),
		Prompt:     lipgloss.NewStyle().Foreground(lipgloss.Color(colorScheme.Color4)),
		Error:      lipgloss.NewStyle().Foreground(lipgloss.Color(colorScheme.Error)),
		Foreground: lipgloss.NewStyle().Foreground(lipgloss.Color(colorScheme.Foreground)),
		Grey:       lipgloss.NewStyle().Foreground(lipgloss.Color(colorScheme.Grey)),
	}
}

func MakeButterfishConfig() *ButterfishConfig {
	colorScheme := &GruvboxDark

	return &ButterfishConfig{
		Verbose:     false,
		ColorScheme: colorScheme,
		Styles:      ColorSchemeToStyles(colorScheme),
	}
}

// Let's initialize our prompts. If we have a prompt library file, we'll load it.
// Either way, we'll then add the default prompts to the library, replacing
// loaded prompts only if OkToReplace is set on them. Then we save the library
// at the same path.
func NewDiskPromptLibrary(path string, verbose bool, writer io.Writer) (*prompt.DiskPromptLibrary, error) {
	promptLibrary := prompt.NewPromptLibrary(path, verbose, writer)
	loaded := false

	if promptLibrary.LibraryFileExists() {
		err := promptLibrary.Load()
		if err != nil {
			return nil, err
		}
		loaded = true
	}
	promptLibrary.ReplacePrompts(prompt.DefaultPrompts)
	promptLibrary.Save()

	if !loaded {
		fmt.Fprintf(writer, "Wrote prompt library at %s\n", path)
	}

	return promptLibrary, nil
}

func RunConsoleClient(ctx context.Context, args []string) error {
	client, err := runIPCClient(ctx)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)

	return wrapCommand(ctx, cancel, args, client) // this is blocking
}

func RunConsole(ctx context.Context, config *ButterfishConfig) error {
	//initLogging(ctx)
	ctx, cancel := context.WithCancel(ctx)

	// initialize console UI
	consoleCommand := make(chan string)
	cmdCallback := func(cmd string) {
		consoleCommand <- cmd
	}
	exitCallback := func() {
		cancel()
	}
	configCallback := func(model console.ConsoleModel) console.ConsoleModel {
		model.SetStyles(config.Styles.Prompt, config.Styles.Question)
		return model
	}
	cons := console.NewConsoleProgram(configCallback, cmdCallback, exitCallback)

	llmClient, err := initLLM(config)
	if err != nil {
		return err
	}

	clientController := RunIPCServer(ctx, cons)

	promptLibrary, err := initPromptLibrary(config)
	if err != nil {
		return err
	}

	butterfishCtx := ButterfishCtx{
		Ctx:              ctx,
		Cancel:           cancel,
		PromptLibrary:    promptLibrary,
		InConsoleMode:    true,
		Config:           config,
		LLMClient:        llmClient,
		Out:              cons,
		ConsoleCmdChan:   consoleCommand,
		ClientController: clientController,
	}

	// this is blocking
	butterfishCtx.serverMultiplexer()

	return nil
}

func initLLM(config *ButterfishConfig) (LLM, error) {
	if config.OpenAIToken == "" && config.LLMClient != nil {
		return nil, errors.New("Must provide either an OpenAI Token or an LLM client.")
	} else if config.OpenAIToken != "" && config.LLMClient != nil {
		return nil, errors.New("Must provide either an OpenAI Token or an LLM client, not both.")
	} else if config.OpenAIToken != "" {
		verboseWriter := util.NewStyledWriter(os.Stdout, config.Styles.Grey)
		return NewGPT(config.OpenAIToken, config.Verbose, verboseWriter), nil
	} else {
		return config.LLMClient, nil
	}
}

func initPromptLibrary(config *ButterfishConfig) (PromptLibrary, error) {
	verboseWriter := util.NewStyledWriter(os.Stdout, config.Styles.Grey)

	if config.PromptLibrary != nil {
		return config.PromptLibrary, nil
	}

	promptPath, err := homedir.Expand(config.PromptLibraryPath)
	if err != nil {
		return nil, err
	}

	return NewDiskPromptLibrary(promptPath, config.Verbose, verboseWriter)
}

func NewButterfish(ctx context.Context, config *ButterfishConfig) (*ButterfishCtx, error) {
	llmClient, err := initLLM(config)
	if err != nil {
		return nil, err
	}

	promptLibrary, err := initPromptLibrary(config)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(ctx)

	butterfishCtx := &ButterfishCtx{
		Ctx:           ctx,
		Cancel:        cancel,
		PromptLibrary: promptLibrary,
		InConsoleMode: false,
		Config:        config,
		LLMClient:     llmClient,
		Out:           os.Stdout,
	}

	return butterfishCtx, nil
}
