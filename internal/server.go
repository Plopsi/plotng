package internal

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ricochet2200/go-disk-usage/du"
)

type Server struct {
	config               *PlotConfig
	active               map[int64]*ActivePlot
	archive              []*ActivePlot
	currentTemp          int
	currentTarget        int
	targetDelayStartTime time.Time
	lock                 sync.RWMutex
}

func (server *Server) ProcessLoop(configPath string, port int) {
	gob.Register(Msg{})
	gob.Register(ActivePlot{})
	go func() {
		if err := http.ListenAndServe(fmt.Sprintf(":%d", port), server); err != nil {
			log.Fatalf("Failed to start webserver: %s", err)
		}
	}()

	server.config = &PlotConfig{
		ConfigPath: configPath,
	}
	server.active = map[int64]*ActivePlot{}
	server.createPlot(time.Now())
	ticker := time.NewTicker(time.Minute)
	for t := range ticker.C {
		server.createPlot(t)
	}
}

func (server *Server) createPlot(t time.Time) {
	if server.config.ProcessConfig() {
		server.targetDelayStartTime = time.Time{} // reset delay if new config was loaded
	}
	if server.config.CurrentConfig != nil {
		server.config.Lock.RLock()
		if len(server.active) < server.config.CurrentConfig.NumberOfParallelPlots {
			server.createNewPlot(server.config.CurrentConfig)
		}
		server.config.Lock.RUnlock()
	}
	fmt.Printf("%s, %d Active Plots\n", t.Format("2006-01-02 15:04:05"), len(server.active))
	for _, plot := range server.active {
		fmt.Print(plot.String(server.config.CurrentConfig.ShowPlotLog))
		if plot.State == PlotFinished || plot.State == PlotError {
			server.archive = append(server.archive, plot)
			delete(server.active, plot.PlotId)
		}
	}
	fmt.Println(" ")
}

func (server *Server) createNewPlot(config *Config) {
	defer server.lock.Unlock()
	server.lock.Lock()
	if len(config.TempDirectory) == 0 || len(config.TargetDirectory) == 0 {
		return
	}

	if time.Now().Before(server.targetDelayStartTime) {
		log.Printf("Waiting until %s", server.targetDelayStartTime.Format("2006-01-02 15:04:05"))
		return
	}

	if server.currentTarget >= len(config.TargetDirectory) {
		server.currentTarget = 0
		server.targetDelayStartTime = time.Now().Add(time.Duration(config.StaggeringDelay) * time.Minute)
		return
	}
	if server.currentTemp >= len(config.TempDirectory) {
		server.currentTemp = 0
	}
	if config.MaxActivePlotPerPhase1 > 0 {
		getPhase1 := func(plot *ActivePlot) bool {
			if strings.HasPrefix(plot.Phase, "1/4") {
				return true
			}
			return false
		}

		var sum int

		for _, plot := range server.active {
			if getPhase1(plot) {
				sum++
			}
		}

		if config.MaxActivePlotPerPhase1 <= sum {
			log.Printf("Skipping, Too many active plots in Phase 1: %d", sum)
			return
		}
	}
	server.currentTemp++
	if server.currentTemp >= len(config.TempDirectory) {
		server.currentTemp = 0
	}
	plotDir := config.TempDirectory[server.currentTemp]
	if config.MaxActivePlotPerTemp > 0 && int(server.countActiveTemp(plotDir)) >= config.MaxActivePlotPerTemp {
		log.Printf("Skipping [%s], too many active plots: %d", plotDir, int(server.countActiveTemp(plotDir)))
		return
	}
	targetDir := config.TargetDirectory[server.currentTarget]
	server.currentTarget++

	if config.MaxActivePlotPerTarget > 0 && int(server.countActiveTarget(targetDir)) >= config.MaxActivePlotPerTarget {
		log.Printf("Skipping [%s], too many active plots: %d", targetDir, int(server.countActiveTarget(targetDir)))
		return
	}

	server.targetDelayStartTime = time.Now().Add(time.Duration(config.DelaysBetweenPlot) * time.Minute)

	targetDirSpace := server.getDiskSpaceAvailable(targetDir)
	if config.DiskSpaceCheck && (server.countActiveTarget(targetDir)+1)*PLOT_SIZE > targetDirSpace {
		log.Printf("Skipping [%s], Not enough space: %d", targetDir, targetDirSpace/GB)
		return
	}

	//check temp space
	plotDirSpace := server.getDiskSpaceAvailable(plotDir)
	log.Printf("Dispace %s: %d", plotDir, plotDirSpace/GB)

	var plotSizes map[string]int64 = make(map[string]int64)

	dir, err := os.Open(plotDir)

	if err != nil {
		log.Printf("Error opening directory")
		return
	}
	defer dir.Close()

	files, err := dir.Readdir(-1)
	if err != nil {
		log.Printf("Error reading directory")
		return
	}

	var plotFileRegEx = regexp.MustCompile(`k\d{2}-\d{4}-\d{2}-\d{2}-\d{2}-\d{2}-([a-z0-9]{64})`)
	for _, file := range files {
		if file.IsDir() || file.Name() == "." || file.Name() == ".." {
			continue
		}

		if plotFileRegEx.MatchString(file.Name()) {
			plotId := plotFileRegEx.FindStringSubmatch(file.Name())[1]
			plotSizes[plotId] += file.Size()
		}
	}

	for key, size := range plotSizes {
		log.Printf("Size of %s - %d", key, uint64(size)/GB)
	}

	var phase12MaxPlotSizes int64 = 0
	var phase12CurrPlotSizes int64 = 0
	for _, plot := range server.active {
		if plot.PlotDir == plotDir {
			phase := server.getPlotPhase(plot)

			switch phase {
			case 1:
				log.Printf("Plot %s in Phase 1", plot.Id)
				phase12MaxPlotSizes += int64(TMP_SIZE)
				phase12CurrPlotSizes += plotSizes[plot.Id]
			case 2:
				log.Printf("Plot %s in Phase 2", plot.Id)
				phase12MaxPlotSizes += int64(TMP_SIZE)
				phase12CurrPlotSizes += plotSizes[plot.Id]
			case 3:
				log.Printf("Plot %s in Phase 3", plot.Id)
			case 4:
				log.Printf("Plot %s in Phase 4", plot.Id)
			default:
				log.Printf("Plot %s in Phase UKNOWN", plot.Id)
			}
		}
	}

	plotDirSpace += uint64(phase12CurrPlotSizes)
	if uint64(phase12MaxPlotSizes) < plotDirSpace {
		plotDirSpace -= uint64(phase12MaxPlotSizes)
	} else {
		plotDirSpace = 0
	}

	if plotDirSpace < TMP_SIZE {
		log.Printf("Skipping new plot, not enough space on temp for new")
		log.Printf("Wanted %d had %d", TMP_SIZE, plotDirSpace)
		return
	} else {
		log.Printf("Enough Space wanted %d had %d", TMP_SIZE, plotDirSpace)
	}
	//END - check temp space

	t := time.Now()
	plot := &ActivePlot{
		PlotId:           t.Unix(),
		TargetDir:        targetDir,
		PlotDir:          plotDir,
		Fingerprint:      config.Fingerprint,
		FarmerPublicKey:  config.FarmerPublicKey,
		PoolPublicKey:    config.PoolPublicKey,
		Threads:          config.Threads,
		Buffers:          config.Buffers,
		PlotSize:         config.PlotSize,
		DisableBitField:  config.DisableBitField,
		UseTargetForTmp2: config.UseTargetForTmp2,
		BucketSize:       config.BucketSize,
		SavePlotLogDir:   config.SavePlotLogDir,
		Phase:            "NA",
		Tail:             nil,
		State:            PlotRunning,
	}
	server.active[plot.PlotId] = plot
	go plot.RunPlot()
}

func (server *Server) getPlotPhase(plot *ActivePlot) uint {
	if strings.HasPrefix(plot.Phase, "1/4") {
		return 1
	}

	if strings.HasPrefix(plot.Phase, "2/4") {
		return 2
	}

	if strings.HasPrefix(plot.Phase, "3/4") {
		return 3
	}

	if strings.HasPrefix(plot.Phase, "4/4") {
		return 4
	}

	return 0
}

func (server *Server) countActiveTarget(path string) (count uint64) {
	for _, plot := range server.active {
		if plot.TargetDir == path {
			count++
		}
	}
	return
}

func (server *Server) countActiveTemp(path string) (count uint64) {
	for _, plot := range server.active {
		if plot.PlotDir == path {
			count++
		}
	}
	return
}

func (server *Server) getDiskSpaceAvailable(path string) uint64 {
	d := du.NewDiskUsage(path)
	return d.Available()
}

func (server *Server) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	log.Printf("New query: %s -  %s", req.Method, req.URL.String())
	defer server.lock.RUnlock()
	server.lock.RLock()

	switch req.Method {
	case "GET":
		var msg Msg
		msg.TargetDirs = map[string]uint64{}
		msg.TempDirs = map[string]uint64{}
		for _, v := range server.active {
			msg.Actives = append(msg.Actives, v)
		}
		for _, v := range server.archive {
			msg.Archived = append(msg.Archived, v)
		}
		if server.config.CurrentConfig != nil {
			for _, dir := range server.config.CurrentConfig.TargetDirectory {
				msg.TargetDirs[dir] = server.getDiskSpaceAvailable(dir)
			}
			for _, dir := range server.config.CurrentConfig.TempDirectory {
				msg.TempDirs[dir] = server.getDiskSpaceAvailable(dir)
			}
		}
		var buf bytes.Buffer
		enc := gob.NewEncoder(&buf)
		if err := enc.Encode(msg); err == nil {
			resp.WriteHeader(http.StatusOK)
			resp.Write(buf.Bytes())
		} else {
			resp.WriteHeader(http.StatusInternalServerError)
			log.Printf("Failed to encode message: %s", err)
		}
	case "DELETE":
		for _, v := range server.active {
			if v.Id == req.RequestURI {
				v.process.Kill()
				v.State = PlotKilled
			}
		}
	}
}

type Msg struct {
	Actives    []*ActivePlot
	Archived   []*ActivePlot
	TempDirs   map[string]uint64
	TargetDirs map[string]uint64
}
