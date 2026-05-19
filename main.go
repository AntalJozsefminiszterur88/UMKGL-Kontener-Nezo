package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	dockercontainer "github.com/docker/docker/api/types/container"
	dockerevents "github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

const (
	containerListTimeout     = 10 * time.Second
	containerActionTimeout   = 30 * time.Second
	containerStopTimeoutSecs = 5
	fallbackRefreshInterval  = 30 * time.Second
	dockerEventRetryDelay    = 2 * time.Second
	groupStatusFlashDuration = 6 * time.Second

	logTailLines         = 200
	logBufferLines       = 300
	logRefreshInterval   = 1500 * time.Millisecond
	logReadTimeout       = 5 * time.Second
	logMaxBytes          = 2 * 1024 * 1024
	logMaxLineRunes      = 500
	logScannerBufferSize = 64 * 1024
	logScannerMaxToken   = 1024 * 1024
)

var dockerClient *client.Client
var ansiEscapePattern = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

var errLogTooLarge = errors.New("log too large")

type groupStatus struct {
	Busy       bool
	Text       string
	Importance widget.Importance
}

type containerGroup struct {
	Key     string
	Label   string
	Indices []int
}

type groupViewModel struct {
	Key            string
	Label          string
	StatusText     string
	Importance     widget.Importance
	IDs            []string
	Names          []string
	DisableStart   bool
	DisableStop    bool
	DisableRestart bool
}

type uiState struct {
	content    *fyne.Container
	list       *widget.List
	refreshBtn *widget.Button

	rowsMu sync.RWMutex
	rows   []groupViewModel

	statusMu      sync.RWMutex
	groupStatuses map[string]groupStatus

	refreshMu     sync.Mutex
	refreshing    bool
	refreshQueued bool
}

func main() {
	var err error
	dockerClient, err = client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatal(err)
	}

	a := app.NewWithID("hu.umkgl.kontener.nezo")
	a.Settings().SetTheme(theme.DarkTheme())
	a.SetIcon(theme.ComputerIcon())

	w := a.NewWindow("UMKGL Konténer Néző")
	w.Resize(fyne.NewSize(760, 480))

	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ui := &uiState{
		groupStatuses: make(map[string]groupStatus),
	}
	ui.list = widget.NewList(
		func() int {
			return ui.rowCount()
		},
		func() fyne.CanvasObject {
			return newGroupRowWidget(ui)
		},
		func(id widget.ListItemID, object fyne.CanvasObject) {
			row, ok := object.(*groupRowWidget)
			if !ok {
				return
			}
			row.bind(ui.rowAt(id))
		},
	)
	ui.content = container.NewMax(ui.list)

	refreshBtn := widget.NewButtonWithIcon("Frissítés", theme.ViewRefreshIcon(), ui.requestRefresh)
	ui.refreshBtn = refreshBtn

	topBar := container.NewHBox(
		widget.NewLabelWithStyle("Docker Konténerek", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		layout.NewSpacer(),
		refreshBtn,
	)
	mainContent := container.NewBorder(topBar, nil, nil, nil, ui.content)
	w.SetContent(mainContent)

	if desk, ok := a.(desktop.App); ok {
		m := fyne.NewMenu(
			"UMKGL Konténer Néző",
			fyne.NewMenuItem("Megnyitás", func() {
				w.Show()
			}),
			fyne.NewMenuItem("Kilépés", func() {
				cancel()
				a.Quit()
			}),
		)
		desk.SetSystemTrayMenu(m)
		desk.SetSystemTrayIcon(theme.ComputerIcon())
	}

	w.SetCloseIntercept(func() {
		w.Hide()
	})

	ui.refreshNow()

	go watchDockerEvents(appCtx, ui)
	go watchFallbackRefresh(appCtx, ui)

	w.ShowAndRun()
}

func (s *uiState) setRefreshButtonBusy(busy bool) {
	if s.refreshBtn == nil {
		return
	}

	fyne.Do(func() {
		if busy {
			s.refreshBtn.Disable()
			s.refreshBtn.SetText("Frissítés...")
			return
		}

		s.refreshBtn.SetText("Frissítés")
		s.refreshBtn.Enable()
	})
}

func (s *uiState) rowCount() int {
	s.rowsMu.RLock()
	defer s.rowsMu.RUnlock()
	return len(s.rows)
}

func (s *uiState) rowAt(index int) groupViewModel {
	s.rowsMu.RLock()
	defer s.rowsMu.RUnlock()

	if index < 0 || index >= len(s.rows) {
		return groupViewModel{}
	}

	return s.rows[index]
}

func (s *uiState) setRows(rows []groupViewModel) {
	s.rowsMu.Lock()
	s.rows = rows
	s.rowsMu.Unlock()
}

func (s *uiState) showMainView(view fyne.CanvasObject) {
	if s.content == nil {
		return
	}

	s.content.Objects = []fyne.CanvasObject{view}
	s.content.Refresh()
}

func (s *uiState) setGroupStatus(key string, status groupStatus) {
	s.statusMu.Lock()
	s.groupStatuses[key] = status
	s.statusMu.Unlock()
}

func (s *uiState) clearGroupStatus(key string) {
	s.statusMu.Lock()
	delete(s.groupStatuses, key)
	s.statusMu.Unlock()
}

func (s *uiState) getGroupStatus(key string) (groupStatus, bool) {
	s.statusMu.RLock()
	status, ok := s.groupStatuses[key]
	s.statusMu.RUnlock()
	return status, ok
}

func (s *uiState) flashGroupStatus(key, text string, importance widget.Importance) {
	s.setGroupStatus(key, groupStatus{
		Text:       text,
		Importance: importance,
	})
	s.requestRefresh()

	go func() {
		time.Sleep(groupStatusFlashDuration)

		s.statusMu.Lock()
		current, ok := s.groupStatuses[key]
		if ok && !current.Busy && current.Text == text {
			delete(s.groupStatuses, key)
		}
		s.statusMu.Unlock()

		s.requestRefresh()
	}()
}

func (s *uiState) refreshNow() {
	containers, err := listContainers()
	if err != nil {
		s.showMainView(buildDockerErrorView(err))
		return
	}

	rows := buildGroupViewModels(s, containers)
	if len(rows) == 0 {
		s.showMainView(buildEmptyView())
		return
	}

	s.setRows(rows)
	s.list.Refresh()
	s.showMainView(s.list)
}

func (s *uiState) requestRefresh() {
	s.refreshMu.Lock()
	if s.refreshing {
		s.refreshQueued = true
		s.refreshMu.Unlock()
		return
	}
	s.refreshing = true
	s.refreshMu.Unlock()

	s.setRefreshButtonBusy(true)

	go func() {
		defer func() {
			s.refreshMu.Lock()
			queued := s.refreshQueued
			s.refreshQueued = false
			s.refreshing = false
			s.refreshMu.Unlock()

			if queued {
				s.requestRefresh()
				return
			}

			s.setRefreshButtonBusy(false)
		}()

		containers, err := listContainers()
		fyne.DoAndWait(func() {
			if err != nil {
				s.showMainView(buildDockerErrorView(err))
			} else {
				rows := buildGroupViewModels(s, containers)
				if len(rows) == 0 {
					s.showMainView(buildEmptyView())
				} else {
					s.setRows(rows)
					s.list.Refresh()
					s.showMainView(s.list)
				}
			}
		})
	}()
}

func listContainers() ([]dockercontainer.Summary, error) {
	ctx, cancel := context.WithTimeout(context.Background(), containerListTimeout)
	defer cancel()

	return dockerClient.ContainerList(ctx, dockercontainer.ListOptions{All: true})
}

func buildDockerErrorView(err error) fyne.CanvasObject {
	return container.NewVBox(widget.NewLabel("Hiba a Docker csatlakozáskor:\n" + err.Error()))
}

func buildEmptyView() fyne.CanvasObject {
	return container.NewVBox(widget.NewLabel("Nincsenek konténerek."))
}

func buildGroupViewModels(ui *uiState, containers []dockercontainer.Summary) []groupViewModel {
	groupsMap := make(map[string]*containerGroup)

	for i, ctr := range containers {
		project := ctr.Labels["com.docker.compose.project"]
		groupKey := "container:" + ctr.ID
		groupLabel := containerDisplayName(ctr)

		if project != "" {
			groupKey = "compose:" + project
			groupLabel = project + " (Compose)"
		}

		group := groupsMap[groupKey]
		if group == nil {
			group = &containerGroup{
				Key:   groupKey,
				Label: groupLabel,
			}
			groupsMap[groupKey] = group
		}

		group.Indices = append(group.Indices, i)
	}

	groups := make([]containerGroup, 0, len(groupsMap))
	for _, group := range groupsMap {
		groups = append(groups, *group)
	}

	sort.Slice(groups, func(i, j int) bool {
		return strings.ToLower(groups[i].Label) < strings.ToLower(groups[j].Label)
	})

	rows := make([]groupViewModel, 0, len(groups))

	for _, group := range groups {
		allRunning := true
		allStopped := true
		ids := make([]string, 0, len(group.Indices))
		names := make([]string, 0, len(group.Indices))

		for _, idx := range group.Indices {
			ctr := containers[idx]
			if ctr.State != "running" {
				allRunning = false
			}
			if ctr.State == "running" {
				allStopped = false
			}

			ids = append(ids, ctr.ID)
			names = append(names, containerDisplayName(ctr))
		}

		statusText, importance := buildGroupStatus(containers, group.Indices, allRunning, allStopped)
		statusOverride, hasOverride := ui.getGroupStatus(group.Key)
		if hasOverride {
			statusText = statusOverride.Text
			importance = statusOverride.Importance
		}

		rows = append(rows, groupViewModel{
			Key:            group.Key,
			Label:          group.Label,
			StatusText:     statusText,
			Importance:     importance,
			IDs:            ids,
			Names:          names,
			DisableStart:   (len(ids) == 0) || (hasOverride && statusOverride.Busy) || allRunning,
			DisableStop:    (len(ids) == 0) || (hasOverride && statusOverride.Busy) || allStopped,
			DisableRestart: (len(ids) == 0) || (hasOverride && statusOverride.Busy) || allStopped,
		})
	}

	return rows
}

func buildGroupStatus(containers []dockercontainer.Summary, indices []int, allRunning, allStopped bool) (string, widget.Importance) {
	statusText := "vegyes"
	importance := widget.WarningImportance

	if allRunning {
		statusText = "running"
		importance = widget.SuccessImportance
	} else if allStopped {
		statusText = "exited"
		importance = widget.DangerImportance
	}

	if len(indices) > 1 {
		statusText = fmt.Sprintf("%s (%d konténer)", statusText, len(indices))
		if allRunning {
			statusText = fmt.Sprintf("%s - %s", statusText, containers[indices[0]].Status)
		}
		return statusText, importance
	}

	return containers[indices[0]].Status, importance
}

type groupRowWidget struct {
	widget.BaseWidget

	ui *uiState

	nameLabel   *widget.Label
	statusLabel *widget.Label
	startBtn    *widget.Button
	stopBtn     *widget.Button
	restartBtn  *widget.Button
	logBtn      *widget.Button
}

func newGroupRowWidget(ui *uiState) *groupRowWidget {
	row := &groupRowWidget{
		ui:          ui,
		nameLabel:   widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		statusLabel: widget.NewLabel(""),
		startBtn:    widget.NewButtonWithIcon("", theme.MediaPlayIcon(), nil),
		stopBtn:     widget.NewButtonWithIcon("", theme.MediaStopIcon(), nil),
		restartBtn:  widget.NewButtonWithIcon("", theme.ViewRefreshIcon(), nil),
		logBtn:      widget.NewButtonWithIcon("", theme.DocumentIcon(), nil),
	}
	row.ExtendBaseWidget(row)
	return row
}

func (r *groupRowWidget) CreateRenderer() fyne.WidgetRenderer {
	buttons := container.NewHBox(r.startBtn, r.stopBtn, r.restartBtn, r.logBtn)
	content := container.NewVBox(
		container.NewBorder(nil, nil, r.nameLabel, buttons, r.statusLabel),
		widget.NewSeparator(),
	)
	return widget.NewSimpleRenderer(content)
}

func (r *groupRowWidget) bind(group groupViewModel) {
	r.nameLabel.SetText(group.Label)
	r.statusLabel.SetText(group.StatusText)
	r.statusLabel.Importance = group.Importance

	groupIDs := append([]string(nil), group.IDs...)
	groupNames := append([]string(nil), group.Names...)
	groupKey := group.Key
	groupLabel := group.Label

	r.startBtn.OnTapped = func() {
		runGroupAction(
			r.ui,
			groupKey,
			"Indítás folyamatban...",
			"Indítás sikertelen",
			groupIDs,
			func() {
				r.setBusyLocally("Indítás folyamatban...")
			},
			func(ctx context.Context, id string) error {
				return dockerClient.ContainerStart(ctx, id, dockercontainer.StartOptions{})
			},
		)
	}

	r.stopBtn.OnTapped = func() {
		runGroupAction(
			r.ui,
			groupKey,
			"Leállítás folyamatban...",
			"Leállítás sikertelen",
			groupIDs,
			func() {
				r.setBusyLocally("Leállítás folyamatban...")
			},
			func(ctx context.Context, id string) error {
				return dockerClient.ContainerStop(ctx, id, newStopOptions())
			},
		)
	}

	r.restartBtn.OnTapped = func() {
		runGroupAction(
			r.ui,
			groupKey,
			"Újraindítás folyamatban...",
			"Újraindítás sikertelen",
			groupIDs,
			func() {
				r.setBusyLocally("Újraindítás folyamatban...")
			},
			func(ctx context.Context, id string) error {
				return dockerClient.ContainerRestart(ctx, id, newStopOptions())
			},
		)
	}

	r.logBtn.OnTapped = func() {
		if len(groupIDs) == 1 {
			showLogs(groupIDs[0], groupNames[0])
			return
		}

		showLogPicker(groupLabel, groupIDs, groupNames)
	}

	if group.DisableStart {
		r.startBtn.Disable()
	} else {
		r.startBtn.Enable()
	}

	if group.DisableStop {
		r.stopBtn.Disable()
	} else {
		r.stopBtn.Enable()
	}

	if group.DisableRestart {
		r.restartBtn.Disable()
	} else {
		r.restartBtn.Enable()
	}

	r.logBtn.Enable()
	r.Refresh()
}

func (r *groupRowWidget) setBusyLocally(text string) {
	r.statusLabel.SetText(text)
	r.statusLabel.Importance = widget.WarningImportance
	r.startBtn.Disable()
	r.stopBtn.Disable()
	r.restartBtn.Disable()
	r.Refresh()
}

func containerDisplayName(ctr dockercontainer.Summary) string {
	if len(ctr.Names) > 0 {
		return strings.TrimPrefix(ctr.Names[0], "/")
	}

	if len(ctr.ID) > 12 {
		return ctr.ID[:12]
	}

	return ctr.ID
}

func runGroupAction(
	ui *uiState,
	groupKey string,
	pendingText string,
	errorPrefix string,
	ids []string,
	setBusyLocally func(),
	action func(context.Context, string) error,
) {
	setBusyLocally()
	ui.setGroupStatus(groupKey, groupStatus{
		Busy:       true,
		Text:       pendingText,
		Importance: widget.WarningImportance,
	})

	go func() {
		var waitGroup sync.WaitGroup
		errCh := make(chan error, len(ids))

		for _, id := range ids {
			waitGroup.Add(1)
			go func(containerID string) {
				defer waitGroup.Done()

				ctx, cancel := context.WithTimeout(context.Background(), containerActionTimeout)
				err := action(ctx, containerID)
				cancel()
				if err != nil {
					errCh <- err
				}
			}(id)
		}

		waitGroup.Wait()
		close(errCh)

		for err := range errCh {
			ui.flashGroupStatus(groupKey, fmt.Sprintf("%s: %v", errorPrefix, err), widget.DangerImportance)
			return
		}

		ui.clearGroupStatus(groupKey)
		ui.requestRefresh()
	}()
}

func watchDockerEvents(ctx context.Context, ui *uiState) {
	eventFilters := filters.NewArgs(filters.Arg("type", "container"))

	for ctx.Err() == nil {
		messages, errs := dockerClient.Events(ctx, dockerevents.ListOptions{Filters: eventFilters})

	streamLoop:
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-messages:
				if !ok {
					break streamLoop
				}
				ui.requestRefresh()
			case err, ok := <-errs:
				if !ok || err == nil || errors.Is(err, context.Canceled) {
					break streamLoop
				}

				log.Printf("docker event stream hiba: %v", err)
				break streamLoop
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(dockerEventRetryDelay):
		}
	}
}

func watchFallbackRefresh(ctx context.Context, ui *uiState) {
	ticker := time.NewTicker(fallbackRefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ui.requestRefresh()
		}
	}
}

func showLogPicker(groupLabel string, ids, names []string) {
	picker := fyne.CurrentApp().NewWindow("Napló kiválasztása")
	picker.Resize(fyne.NewSize(420, 280))

	content := container.NewVBox(
		widget.NewLabelWithStyle(groupLabel, fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewLabel("Válaszd ki, melyik konténer naplóját nyissam meg."),
		widget.NewSeparator(),
	)

	for i := range names {
		id := ids[i]
		name := names[i]

		content.Add(widget.NewButton(name, func() {
			showLogs(id, name)
			picker.Close()
		}))
	}

	picker.SetContent(container.NewVScroll(content))
	picker.Show()
}

func showLogs(containerID, containerName string) {
	logWindow := fyne.CurrentApp().NewWindow(fmt.Sprintf("Napló: %s", containerName))
	logWindow.Resize(fyne.NewSize(900, 620))

	statusLabel := widget.NewLabel("A napló vége betöltés alatt...")
	statusLabel.Importance = widget.WarningImportance

	lines := []string{"A napló vége betöltés alatt..."}
	logList := widget.NewList(
		func() int {
			return len(lines)
		},
		func() fyne.CanvasObject {
			label := widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Monospace: true})
			label.Wrapping = fyne.TextWrapOff
			label.Selectable = true
			return label
		},
		func(id widget.ListItemID, object fyne.CanvasObject) {
			object.(*widget.Label).SetText(lines[id])
		},
	)

	logWindow.SetContent(container.NewBorder(statusLabel, nil, nil, nil, logList))
	logWindow.Show()

	ctx, cancel := context.WithCancel(context.Background())
	logWindow.SetOnClosed(cancel)

	go pollLogs(ctx, containerID, statusLabel, logList, &lines)
}

func pollLogs(ctx context.Context, containerID string, statusLabel *widget.Label, logList *widget.List, lines *[]string) {
	isTTY := false
	if inspect, err := inspectContainer(ctx, containerID); err == nil && inspect.Config != nil {
		isTTY = inspect.Config.Tty
	}

	ticker := time.NewTicker(logRefreshInterval)
	defer ticker.Stop()

	var previousSnapshot string

	for {
		currentLines, truncated, err := fetchLogLines(ctx, containerID, isTTY)
		if err != nil {
			if ctx.Err() != nil {
				return
			}

			fyne.Do(func() {
				statusLabel.SetText(fmt.Sprintf("Hiba a napló olvasásakor: %v", err))
				statusLabel.Importance = widget.DangerImportance
				*lines = []string{"A napló jelenleg nem tölthető be."}
				logList.Refresh()
			})
		} else {
			snapshot := strings.Join(currentLines, "\n")
			statusText := fmt.Sprintf("Utolsó %d sor, automatikus frissítés", logTailLines)
			if truncated {
				statusText += " (rövidítve)"
			}

			if snapshot != previousSnapshot {
				previousSnapshot = snapshot
				fyne.Do(func() {
					statusLabel.SetText(statusText)
					statusLabel.Importance = widget.MediumImportance
					*lines = currentLines
					logList.Refresh()
					logList.ScrollToBottom()
				})
			} else {
				fyne.Do(func() {
					statusLabel.SetText(statusText)
					statusLabel.Importance = widget.MediumImportance
				})
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func fetchLogLines(ctx context.Context, containerID string, isTTY bool) ([]string, bool, error) {
	logOptions := dockercontainer.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       strconv.Itoa(logTailLines),
	}

	readCtx, cancel := context.WithTimeout(ctx, logReadTimeout)
	defer cancel()

	reader, err := dockerClient.ContainerLogs(readCtx, containerID, logOptions)
	if err != nil {
		return nil, false, err
	}
	defer reader.Close()

	data, truncated, err := readBoundedLogs(reader, isTTY)
	if err != nil {
		return nil, truncated, err
	}

	lines := parseLogLines(data)
	if len(lines) == 0 {
		lines = []string{"Nincs napló."}
	}

	return lines, truncated, nil
}

func inspectContainer(ctx context.Context, containerID string) (dockercontainer.InspectResponse, error) {
	inspectCtx, cancel := context.WithTimeout(ctx, logReadTimeout)
	defer cancel()

	return dockerClient.ContainerInspect(inspectCtx, containerID)
}

func readBoundedLogs(reader io.Reader, isTTY bool) ([]byte, bool, error) {
	if isTTY {
		raw, err := io.ReadAll(io.LimitReader(reader, logMaxBytes+1))
		if err != nil {
			return nil, false, err
		}
		if len(raw) > logMaxBytes {
			return raw[:logMaxBytes], true, nil
		}
		return raw, false, nil
	}

	buffer := &cappedBuffer{limit: logMaxBytes}
	_, err := stdcopy.StdCopy(buffer, buffer, reader)
	if err != nil && !errors.Is(err, errLogTooLarge) && !errors.Is(err, io.EOF) {
		return nil, buffer.truncated, err
	}

	return buffer.Bytes(), buffer.truncated, nil
}

func parseLogLines(data []byte) []string {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, logScannerBufferSize), logScannerMaxToken)
	scanner.Split(splitLogLines)

	lines := make([]string, 0, logBufferLines)
	previousBlank := false

	for scanner.Scan() {
		line := sanitizeLogLine(scanner.Text())

		if line == "" {
			if previousBlank {
				continue
			}
			previousBlank = true
		} else {
			previousBlank = false
		}

		lines = append(lines, line)
		if len(lines) > logBufferLines {
			lines = lines[len(lines)-logBufferLines:]
		}
	}

	if err := scanner.Err(); err != nil {
		lines = append(lines, fmt.Sprintf("[log feldolgozási hiba] %v", err))
		if len(lines) > logBufferLines {
			lines = lines[len(lines)-logBufferLines:]
		}
	}

	return lines
}

func splitLogLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}

	for i, b := range data {
		if b != '\n' && b != '\r' {
			continue
		}

		advance = i + 1
		if b == '\r' && advance < len(data) && data[advance] == '\n' {
			advance++
		}

		return advance, data[:i], nil
	}

	if atEOF {
		return len(data), data, nil
	}

	return 0, nil, nil
}

func sanitizeLogLine(line string) string {
	line = ansiEscapePattern.ReplaceAllString(line, "")
	line = strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if r < 32 {
			return -1
		}
		return r
	}, line)
	line = strings.TrimRight(line, " ")

	runes := []rune(line)
	if len(runes) > logMaxLineRunes {
		line = string(runes[:logMaxLineRunes]) + " ..."
	}

	return line
}

func newStopOptions() dockercontainer.StopOptions {
	timeout := containerStopTimeoutSecs
	return dockercontainer.StopOptions{Timeout: &timeout}
}

type cappedBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.Buffer.Len()
	if remaining <= 0 {
		b.truncated = true
		return 0, errLogTooLarge
	}

	if len(p) > remaining {
		_, _ = b.Buffer.Write(p[:remaining])
		b.truncated = true
		return remaining, errLogTooLarge
	}

	return b.Buffer.Write(p)
}
