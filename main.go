package main

import (
	"context"
	"errors"
	"html/template"
	"log"
	"math"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/host"
	"github.com/shirou/gopsutil/mem"
	"github.com/shirou/gopsutil/v4/disk"
)

type SystemInfo struct {
	Host      string
	OS        string
	TotalMem  float64
	UsedMem   float64
	CPUModel  string
	CPUCores  int32
	CPUUsed   float64
	TotalDisk uint64
	UsedDisk  float64
}

func GetSystemInfo(cxt context.Context) (*SystemInfo, error) {
	os := runtime.GOOS

	vmMemStat, err := mem.VirtualMemoryWithContext(cxt)
	if err != nil {
		return nil, err
	}
	totalMemory := float64(vmMemStat.Total) / 1024 / 1024 / 1024
	usedMemory := float64(vmMemStat.Used) / 1024 / 1024 / 1024

	hostStat, err := host.InfoWithContext(cxt)
	if err != nil {
		return nil, err
	}

	cpuStat, err := cpu.InfoWithContext(cxt)
	if err != nil {
		return nil, err
	}

	cpuUsed, err := cpu.PercentWithContext(cxt, 0, false)
	if err != nil {
		return nil, err
	}

	diskStat, err := disk.UsageWithContext(cxt, "/")
	if err != nil {
		return nil, err
	}

	totalDisk := diskStat.Total / 1000 / 1000 / 1000

	sysInfo := &SystemInfo{
		Host:      hostStat.Hostname,
		OS:        os,
		TotalMem:  totalMemory,
		UsedMem:   usedMemory,
		CPUModel:  cpuStat[0].ModelName,
		CPUCores:  cpuStat[0].Cores,
		CPUUsed:   cpuUsed[0],
		TotalDisk: totalDisk,
		UsedDisk:  diskStat.UsedPercent,
	}

	return sysInfo, nil
}

type Subscribers struct {
	msgs chan string
}

type Server struct {
	serveMux              http.ServeMux
	maxMessages           int
	subscribersMutex      sync.Mutex
	subscribers           map[*Subscribers]struct{}
	wsUpgrader            websocket.Upgrader
	dashInfoMutex         sync.Mutex
	dashInfo              *DashboardInfo
	dashInfoUpdaterCancel context.CancelFunc
}

type DashboardInfo struct {
	SystemInfo      *SystemInfo
	CPUPercents     []float64
	UpdateTimestamp time.Time
}

type DashboardPage struct {
	Title           string
	DashboardInfo   *DashboardInfo
	UpdateTsRFC3339 string
}

func (s *Server) subscribe(w http.ResponseWriter, r *http.Request) error {
	subscriber := &Subscribers{
		msgs: make(chan string, s.maxMessages),
	}

	s.subscribersMutex.Lock()
	s.subscribers[subscriber] = struct{}{}
	log.Println("added subscriber")

	if len(s.subscribers) == 1 {
		s.dashInfoUpdaterCancel = s.startSysInfoUpdater()
	}

	s.subscribersMutex.Unlock()

	conn, err := s.wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return errors.New("could not upgrade connection to websocket: " + err.Error())
	}

	defer conn.Close()

	// unsubscribe after exiting this function
	defer func() {
		s.subscribersMutex.Lock()
		delete(s.subscribers, subscriber)
		log.Println("removed subscriber")
		if len(s.subscribers) == 0 {
			log.Println("stopping system info updater")

			if s.dashInfoUpdaterCancel == nil {
				log.Panic("dashInfoUpdaterCancel is nil")
			}

			s.dashInfoUpdaterCancel()
			s.dashInfoUpdaterCancel = nil
		}
		s.subscribersMutex.Unlock()
	}()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	for {
		select {
		case msg := <-subscriber.msgs:
			log.Println("sending message to websocket:", subscriber)
			err := conn.WriteMessage(websocket.TextMessage, []byte(msg))
			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					return nil
				}
				return errors.New("could not write message to websocket: " + err.Error())
			}
		case <-ctx.Done():
			log.Println("context done")
			return ctx.Err()
		}
	}
}

func (s *Server) wsUpdate(elementId string, tmpl *template.Template, data any) (string, error) {
	builder := strings.Builder{}

	err := tmpl.Execute(&builder, data)
	if err != nil {
		return "", errors.New("could not execute template: " + err.Error())
	}

	return `<div hx-swap-oob="innerHTML:#` + elementId + `">` + builder.String() + `</div>`, nil
}

func (s *Server) getCurrentDashboardInfo(cxt context.Context) (*DashboardInfo, error) {
	s.dashInfoMutex.Lock()
	defer s.dashInfoMutex.Unlock()

	if s.dashInfo == nil || math.Ceil(time.Since(s.dashInfo.UpdateTimestamp).Seconds()) >= 5 {
		sysInfo, err := GetSystemInfo(cxt)
		if err != nil {
			return nil, err
		}

		cpuPercents, err := cpu.PercentWithContext(cxt, 0, true)
		if err != nil {
			return nil, err
		}

		s.dashInfo = &DashboardInfo{
			SystemInfo:      sysInfo,
			CPUPercents:     cpuPercents,
			UpdateTimestamp: time.Now(),
		}
	}

	return s.dashInfo, nil
}

func (s *Server) startSysInfoUpdater() context.CancelFunc {
	updatedCardTemplate := template.Must(template.ParseFiles("./htmx/updated-card.html"))
	infoCardTemplate := template.Must(template.ParseFiles("./htmx/info-card.html"))
	cpuUsageCardTemplate := template.Must(template.ParseFiles("./htmx/cpu-usage-card.html"))

	cxt, cancel := context.WithCancel(context.Background())

	go func(cxt context.Context) error {
		ticker := time.NewTicker(time.Second * 5)
		defer ticker.Stop()

		for {
			select {
			case <-cxt.Done():
				log.Println("stopped system info updater")
				return cxt.Err()
			case <-ticker.C:
				log.Println("updating system info")

				dashInfo, err := s.getCurrentDashboardInfo(cxt)

				if err != nil {
					log.Println("could not get system info:", err)
				} else {
					var msg string

					out, err := s.wsUpdate("updated-timestamp", updatedCardTemplate, dashInfo.UpdateTimestamp.UTC().Format(time.RFC3339))
					if err != nil {
						log.Println("could not update timestamp:", err)
					} else {
						msg += out
					}

					out, err = s.wsUpdate("cpu-usage", cpuUsageCardTemplate, dashInfo.CPUPercents)
					if err != nil {
						log.Println("could not update cpu usage:", err)
					} else {
						msg += out
					}

					out, err = s.wsUpdate("system-info", infoCardTemplate, dashInfo.SystemInfo)
					if err != nil {
						log.Println("could not update sys info:", err)
					} else {
						msg += out
					}

					s.subscribersMutex.Lock()

					for subscriber := range s.subscribers {
						log.Println("sending message to subscriber", subscriber)
						subscriber.msgs <- msg
					}

					s.subscribersMutex.Unlock()
				}
			}
		}
	}(cxt)

	log.Println("started system info updater")

	return cancel
}

func (s *Server) Start() {
	http.ListenAndServe(":8080", &s.serveMux)
}

func NewServer() *Server {
	s := &Server{
		maxMessages: 10,
		subscribers: make(map[*Subscribers]struct{}),
		wsUpgrader:  websocket.Upgrader{ReadBufferSize: 1024, WriteBufferSize: 1024},
	}

	dashboardTemplate := template.Must(template.ParseFiles(
		"./htmx/base.html",
		"./htmx/dashboard.html",
		"./htmx/info-card.html",
		"./htmx/cpu-usage-card.html",
		"./htmx/updated-card.html",
	))

	s.serveMux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		log.Println("serving dashboard")

		dashInfo, err := s.getCurrentDashboardInfo(r.Context())
		if err != nil {
			log.Println("could not get dashboard info:", err)
		}

		page := &DashboardPage{
			Title:           "Dashboard",
			DashboardInfo:   dashInfo,
			UpdateTsRFC3339: dashInfo.UpdateTimestamp.UTC().Format(time.RFC3339),
		}

		err = dashboardTemplate.Execute(w, page)
		if err != nil {
			log.Println("could not execute template:", err)
		}
	})

	s.serveMux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		err := s.subscribe(w, r)
		if err != nil {
			log.Println(err)
		}
	})

	return s
}

func main() {
	server := NewServer()
	server.Start()
}
