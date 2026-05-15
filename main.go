package main

import (
        "bufio"
        "context"
        "fmt"
        "io"
        "log"
        "sort"
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
        // Connect to native Linux Docker Daemon
        dockerClient, err = client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
        if err != nil {
                log.Fatal(err)
        }

        a := app.NewWithID("hu.umkgl.kontener.nezo")
        a.Settings().SetTheme(theme.DarkTheme())
        a.SetIcon(theme.ComputerIcon())

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
                m := fyne.NewMenu("UMKGL Konténer Néző",
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

        groupsMap := make(map[string][]int)

        for i, ctr := range containers {
                project := ctr.Labels["com.docker.compose.project"]
                var groupName string

                if project != "" {
                        groupName = project + " (Compose)"
                } else {
                        groupName = strings.TrimPrefix(ctr.Names[0], "/")
                }

                groupsMap[groupName] = append(groupsMap[groupName], i)
        }

        var groupNames []string
        for name := range groupsMap {
                groupNames = append(groupNames, name)
        }

        sort.Slice(groupNames, func(i, j int) bool {
                return strings.ToLower(groupNames[i]) < strings.ToLower(groupNames[j])
        })

        list := container.NewVBox()

        for _, gName := range groupNames {
                indices := groupsMap[gName]
                nameLabel := widget.NewLabelWithStyle(gName, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})

                allRunning := true
                allStopped := true
                for _, idx := range indices {
                        if containers[idx].State != "running" {
                                allRunning = false
                        }
                        if containers[idx].State == "running" {
                                allStopped = false
                        }
                }

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
                } else if len(indices) == 1 {
                        statusText = containers[indices[0]].Status
                }

                statusLabel := widget.NewLabel(statusText)
                statusLabel.Importance = importance

                var ids []string
                var names []string
                for _, idx := range indices {
                        ids = append(ids, containers[idx].ID)
                        names = append(names, strings.TrimPrefix(containers[idx].Names[0], "/"))
                }

                startBtn := widget.NewButtonWithIcon("", theme.MediaPlayIcon(), func() {
                        for _, id := range ids {
                                go dockerClient.ContainerStart(ctx, id, dockercontainer.StartOptions{})
                        }
                })
                stopBtn := widget.NewButtonWithIcon("", theme.MediaStopIcon(), func() {
                        for _, id := range ids {
                                go dockerClient.ContainerStop(ctx, id, dockercontainer.StopOptions{})
                        }
                })
                restartBtn := widget.NewButtonWithIcon("", theme.ViewRefreshIcon(), func() {
                        for _, id := range ids {
                                go dockerClient.ContainerRestart(ctx, id, dockercontainer.StopOptions{})
                        }
                })
                logBtn := widget.NewButtonWithIcon("", theme.DocumentIcon(), func() {
                        for i, id := range ids {
                                showLogs(id, names[i])
                        }
                })

                if allRunning {
                        startBtn.Disable()
                } else if allStopped {
                        stopBtn.Disable()
                        restartBtn.Disable()
                }

                buttons := container.NewHBox(startBtn, stopBtn, restartBtn, logBtn)

                row := container.NewBorder(nil, nil, nameLabel, buttons, statusLabel)
                list.Add(row)
                list.Add(widget.NewSeparator())
        }

        if len(groupNames) == 0 {
                list.Add(widget.NewLabel("Nincsenek konténerek."))
        }

        return list
}

func showLogs(containerID, containerName string) {
        logWindow := fyne.CurrentApp().NewWindow(fmt.Sprintf("Napló: %s", containerName))
        logWindow.Resize(fyne.NewSize(800, 600))

        logGrid := widget.NewTextGrid()

        scroll := container.NewScroll(logGrid)
        logWindow.SetContent(scroll)
        logWindow.Show()

        ctx, cancel := context.WithCancel(context.Background())

        logWindow.SetOnClosed(func() {
                cancel()
        })

        go func() {
                logOptions := dockercontainer.LogsOptions{
                        ShowStdout: true,
                        ShowStderr: true,
                        Tail:       "100",
                        Follow:     true, // Real-time follow
                }

                reader, err := dockerClient.ContainerLogs(ctx, containerID, logOptions)
                if err != nil {
                        logGrid.SetText(fmt.Sprintf("Hiba a napló olvasásakor: %v", err))
                        return
                }
                defer reader.Close()

                inspect, err := dockerClient.ContainerInspect(ctx, containerID)
                isTty := err == nil && inspect.Config.Tty

                var lines []string

                // We use a scanner to read lines and append to the text box
                if isTty {
                        scanner := bufio.NewScanner(reader)
                        for scanner.Scan() {
                                select {
                                case <-ctx.Done():
                                        return
                                default:
                                        lines = append(lines, scanner.Text())
                                        if len(lines) > 200 {
                                                lines = lines[len(lines)-200:]
                                        }
                                        logGrid.SetText(strings.Join(lines, "\n"))
                                        scroll.ScrollToBottom()
                                }
                        }
                } else {
                        // stdcopy is needed for non-TTY containers to strip multiplex headers
                        pr, pw := io.Pipe()
                        go func() {
                                defer pw.Close()
                                _, _ = stdcopy.StdCopy(pw, pw, reader)
                        }()

                        scanner := bufio.NewScanner(pr)
                        for scanner.Scan() {
                                select {
                                case <-ctx.Done():
                                        return
                                default:
                                        lines = append(lines, scanner.Text())
                                        if len(lines) > 200 {
                                                lines = lines[len(lines)-200:]
                                        }
                                        logGrid.SetText(strings.Join(lines, "\n"))
                                        scroll.ScrollToBottom()
                                }
                        }
                }
        }()
}