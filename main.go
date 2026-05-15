package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

var dockerClient *client.Client

func main() {
	var err error
	dockerClient, err = client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatal(err)
	}

	a := app.New()
	a.Settings().SetTheme(theme.DarkTheme())

	w := a.NewWindow("UMKGL Konténer Néző")
	w.Resize(fyne.NewSize(600, 400))

	content := container.NewVScroll(container.NewVBox())
	
	refresh := func() {
		content.Content = buildContainerList(w)
		content.Refresh()
	}

	refreshBtn := widget.NewButtonWithIcon("Frissítés", theme.ViewRefreshIcon(), refresh)

	topBar := container.NewHBox(widget.NewLabelWithStyle("Docker Konténerek", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), layout.NewSpacer(), refreshBtn)
	mainContent := container.NewBorder(topBar, nil, nil, nil, content)

	w.SetContent(mainContent)

	if desk, ok := a.(desktop.App); ok {
		m := fyne.NewMenu("UMKGL Konténer",
			fyne.NewMenuItem("Megnyitás", func() {
				w.Show()
			}),
			fyne.NewMenuItem("Kilépés", func() {
				a.Quit()
			}),
		)
		desk.SetSystemTrayMenu(m)
		desk.SetSystemTrayIcon(theme.ComputerIcon())
	}

	w.SetCloseIntercept(func() {
		w.Hide()
	})

	refresh()

	go func() {
		for {
			time.Sleep(3 * time.Second)
			refresh()
		}
	}()

	w.ShowAndRun()
}

func buildContainerList(w fyne.Window) *fyne.Container {
	ctx := context.Background()
	
	containers, err := dockerClient.ContainerList(ctx, dockercontainer.ListOptions{All: true})
	if err != nil {
		return container.NewVBox(widget.NewLabel("Hiba a Docker csatlakozáskor:\n" + err.Error()))
	}

	list := container.NewVBox()

	for _, ctr := range containers {
		name := strings.TrimPrefix(ctr.Names[0], "/")
		status := ctr.Status
		state := ctr.State
		id := ctr.ID

		nameLabel := widget.NewLabelWithStyle(name, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
		statusLabel := widget.NewLabel(status)
		
		if state == "running" {
			statusLabel.Importance = widget.SuccessImportance
		} else {
			statusLabel.Importance = widget.DangerImportance
		}

		startBtn := widget.NewButtonWithIcon("", theme.MediaPlayIcon(), func() {
			go dockerClient.ContainerStart(ctx, id, dockercontainer.StartOptions{})
		})
		stopBtn := widget.NewButtonWithIcon("", theme.MediaStopIcon(), func() {
			go dockerClient.ContainerStop(ctx, id, dockercontainer.StopOptions{})
		})
		restartBtn := widget.NewButtonWithIcon("", theme.ViewRefreshIcon(), func() {
			go dockerClient.ContainerRestart(ctx, id, dockercontainer.StopOptions{})
		})
		logBtn := widget.NewButtonWithIcon("", theme.DocumentIcon(), func() {
			showLogs(w, id, name)
		})

		if state == "running" {
			startBtn.Disable()
		} else {
			stopBtn.Disable()
			restartBtn.Disable()
		}

		buttons := container.NewHBox(startBtn, stopBtn, restartBtn, logBtn)

		row := container.NewBorder(nil, nil, nameLabel, buttons, statusLabel)
		list.Add(row)
		list.Add(widget.NewSeparator())
	}

	if len(containers) == 0 {
		list.Add(widget.NewLabel("Nincsenek konténerek."))
	}

	return list
}

func showLogs(w fyne.Window, containerID, containerName string) {
	ctx := context.Background()
	logOptions := dockercontainer.LogsOptions{ShowStdout: true, ShowStderr: true, Tail: "200"}
	
	reader, err := dockerClient.ContainerLogs(ctx, containerID, logOptions)
	if err != nil {
		return
	}

	outBuf := new(strings.Builder)
	errBuf := new(strings.Builder)
	
	// Determine if container has TTY
	inspect, err := dockerClient.ContainerInspect(ctx, containerID)
	if err == nil && inspect.Config.Tty {
		_, _ = io.Copy(outBuf, reader)
	} else {
		_, _ = stdcopy.StdCopy(outBuf, errBuf, reader)
	}
	reader.Close()

	logText := widget.NewMultiLineEntry()
	logText.SetText(outBuf.String() + errBuf.String())
	logText.Disable() // Read only
	logText.Wrapping = fyne.TextWrapWord
	
	scroll := container.NewScroll(logText)
	
	logWindow := fyne.CurrentApp().NewWindow(fmt.Sprintf("Napló: %s", containerName))
	logWindow.Resize(fyne.NewSize(800, 600))
	logWindow.SetContent(scroll)
	logWindow.Show()
}
