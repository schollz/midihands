package main

import (
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"math"
	"math/rand"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	movingaverage "github.com/RobinUS2/golang-moving-average"
	"github.com/fxsjy/RF.go/RF"
	"github.com/gorilla/websocket"
	"github.com/rakyll/portmidi"
	log "github.com/schollz/logger"
	"gonum.org/v1/gonum/stat"
)

//go:embed static
var static embed.FS

// server
var fsStatic http.Handler

// random forests
var forest [2]*RF.Forest
var classifications [][]string
var classifyCurrent []string

// flags
var flagDebug bool
var flagDontOpen bool
var flagPort int
var flagFrameRate int
var flagSmoothing int

// hand tracks
var mutex sync.Mutex
var ma map[string][]*movingaverage.ConcurrentMovingAverage
var lastMidi [6]int64

// midi
var midiOuts map[string]*portmidi.Stream

func init() {
	flag.BoolVar(&flagDebug, "debug", false, "debug mode")
	flag.IntVar(&flagPort, "port", 8030, "port for server")
	flag.IntVar(&flagSmoothing, "smooth", 10, "number of points in moving average")
	flag.IntVar(&flagFrameRate, "reduce-fps", 90, "reduce frame rate (default 90% of max), [0-100]")
	flag.BoolVar(&flagDontOpen, "dont-open", false, "don't open browser")
}

func main() {
	var err error
	flag.Parse()
	log.SetLevel("info")
	if flagDebug {
		log.SetLevel("debug")
	}
	fmt.Print(`
midihands v0.1
@infinitedigits
              _
            _| |
          _| | |
         | | | |
         | | | | __
         | | | |/  \
         |       /\ \
         |      /  \/
         |      \  /\
         |       \/ /
          \        /
           |     /
           |    |


`)

	ma = make(map[string][]*movingaverage.ConcurrentMovingAverage)
	ma["Left"] = make([]*movingaverage.ConcurrentMovingAverage, 3)
	ma["Right"] = make([]*movingaverage.ConcurrentMovingAverage, 3)
	for i := 0; i < 3; i++ {
		ma["Left"][i] = movingaverage.Concurrent(movingaverage.New(flagSmoothing))
		ma["Right"][i] = movingaverage.Concurrent(movingaverage.New(flagSmoothing))
	}

	err = portmidi.Initialize()
	if err != nil {
		log.Error(err)
		time.Sleep(100 * time.Second)
	}

	midiOuts = make(map[string]*portmidi.Stream)
	for i := 0; i < portmidi.CountDevices(); i++ {
		info := portmidi.Info(portmidi.DeviceID(i))
		if info.IsOutputAvailable {
			fmt.Printf("MIDI available: %s\n", info.Name)
			midiOuts[info.Name], err = portmidi.NewOutputStream(portmidi.DeviceID(i), 1024, 0)
			if err != nil {
				delete(midiOuts, info.Name)
				log.Debug(err)
			}
		}
	}

	fsRoot, err := fs.Sub(static, "static")
	if err != nil {
		return
	}

	fsStatic = http.FileServer(http.FS(fsRoot))
	log.Debugf("listening on :%d", flagPort)
	if !flagDontOpen {
		go func() {
			time.Sleep(200 * time.Millisecond)
			openBrowser(fmt.Sprintf("http://localhost:%d/", flagPort))
		}()
	}
	http.HandleFunc("/", handler)
	http.ListenAndServe(fmt.Sprintf(":%d", flagPort), nil)
}

func handler(w http.ResponseWriter, r *http.Request) {
	t := time.Now().UTC()
	err := handle(w, r)
	if err != nil {
		log.Error(err)
	}
	log.Debugf("%v %v %v %s\n", r.RemoteAddr, r.Method, r.URL.Path, time.Since(t))
}

func handle(w http.ResponseWriter, r *http.Request) (err error) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, PUT, DELETE")
	w.Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization")

	// very special paths
	if r.URL.Path == "/ws" {
		return handleWebsocket(w, r)
	} else {
		if strings.HasSuffix(r.URL.Path, ".js") {
			w.Header().Set("Content-Type", "text/javascript")
		}
		fsStatic.ServeHTTP(w, r)
		return
		var b []byte
		if r.URL.Path == "/" {
			log.Debug("loading index")
			b, err = static.ReadFile("static/hands.html")
			if err != nil {
				return
			}
		} else {
			b, err = static.ReadFile("static" + r.URL.Path)
			if err != nil {
				return
			}
		}
		w.Write(b)
	}

	return
}

type HandData struct {
	MIDIOut            string
	MultiHandLandmarks [][]struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
		Z float64 `json:"z"`
	} `json:"multiHandLandmarks"`
	MultiHandedness []struct {
		Index int     `json:"index"`
		Score float64 `json:"score"`
		Label string  `json:"label"`
	} `json:"multiHandedness"`
}

var wsupgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func handleWebsocket(w http.ResponseWriter, r *http.Request) (err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Debug(r)
		}
	}()
	c, errUpgrade := wsupgrader.Upgrade(w, r, nil)
	if errUpgrade != nil {
		return errUpgrade
	}
	defer c.Close()

	// write the midi data
	for midiName := range midiOuts {
		c.WriteJSON(Message{
			"addMidi", "", midiName,
		})
	}

	for {
		var p HandData
		err := c.ReadJSON(&p)
		if err != nil {
			log.Debug("read:", err)
			break
		} else {
			go processScore(p, c)
		}
	}
	return
}

func maxDistance(numbers []float64) float64 {
	mmax := -1000000.0
	mmin := 10000000.0
	for _, n := range numbers {
		if n < mmin {
			mmin = n
		}
		if n > mmax {
			mmax = n
		}
	}
	log.Debug(mmin, mmax)
	return mmax - mmin
}

func multiply(ds []float64, x float64) []float64 {
	for i, v := range ds {
		ds[i] = v * x
	}
	return ds
}

func normalize(ds []float64) []float64 {
	return multiply(ds, 1.0/maxDistance(ds))
}

func distances(xs []float64, ys []float64) (d []float64) {
	if len(xs) != len(ys) {
		return
	}
	d = make([]float64, len(xs)*(len(xs)-1)/2)
	k := 0
	for i, x1 := range xs {
		for j, x2 := range xs {
			if j <= i {
				continue
			}
			y1 := ys[i]
			y2 := ys[j]
			d[k] = math.Sqrt(math.Pow(x1-x2, 2) + math.Pow(y1-y2, 2))
			k++
		}
	}
	return
}

type Message struct {
	Kind string `json:"kind"`
	Ele  string `json:"ele"`
	Data string `json:"data"`
}

func dist(x1, y1, x2, y2 float64) float64 {
	return math.Sqrt(math.Pow(x1-x2, 2) + math.Pow(y1-y2, 2))
}

func linlin(f, slo, shi, dlo, dhi float64) float64 {
	if f <= slo {
		return dlo
	} else if f >= shi {
		return dhi
	} else {
		return (f-slo)/(shi-slo)*(dhi-dlo) + dlo
	}
}

func processScore(p HandData, c *websocket.Conn) {
	// reduce frame rate a little bit
	if rand.Float64() > float64(flagFrameRate)/100.0 {
		return
	}

	for i, hand := range p.MultiHandLandmarks {
		xs := make([]float64, len(hand))
		ys := make([]float64, len(hand))
		zs := make([]float64, len(hand))
		ws := make([]float64, len(hand))

		for j, coord := range hand {
			xs[j] = coord.X
			ys[j] = coord.Y
			zs[j] = coord.Z
			ws[j] = 1.0
		}
		handedness := strings.ToLower(p.MultiHandedness[i].Label)
		// https://developers.google.com/static/mediapipe/images/solutions/hand-landmarks.png
		spread := dist(hand[0].X, hand[0].Y, hand[12].X, hand[12].Y) / dist(hand[0].X, hand[0].Y, hand[17].X, hand[17].Y)

		meanX, stdX := stat.MeanStdDev(xs, ws)
		meanY, stdY := stat.MeanStdDev(ys, ws)
		_, stdZ := stat.MeanStdDev(zs, ws)
		_ = stdX
		_ = stdY
		_ = stdZ
		ma[p.MultiHandedness[i].Label][0].Add(meanX)
		ma[p.MultiHandedness[i].Label][1].Add(meanY)
		ma[p.MultiHandedness[i].Label][2].Add(spread)

		meanX = ma[p.MultiHandedness[i].Label][0].Avg()
		meanY = 1.0 - ma[p.MultiHandedness[i].Label][1].Avg()
		spread = ma[p.MultiHandedness[i].Label][2].Avg()
		spread = linlin(spread, 0.85, 2.3, 0, 1)
		handNum := 0
		if handedness == "right" {
			handNum = 1
		}

		var newMidi [6]int64
		newMidi[handNum*3+0] = int64(math.Round(meanX * 127))
		newMidi[handNum*3+1] = int64(math.Round(meanY * 127))
		newMidi[handNum*3+2] = int64(math.Round(spread * 127))
		for i, v := range newMidi {
			if v > 127 {
				newMidi[i] = 127
			} else if v < 0 {
				newMidi[i] = 0
			}
		}
		mutex.Lock()
		c.WriteJSON(Message{
			"updateElement",
			handedness,
			fmt.Sprintf("%s<br>x (cc %d)=%d<br>y (cc %d)=%d<br>o (cc %d)=%d", p.MultiHandedness[i].Label,
				handNum*3+0, newMidi[handNum*3+0],
				handNum*3+1, newMidi[handNum*3+1],
				handNum*3+2, newMidi[handNum*3+2]),
		})
		midiVal := int64(math.Round(meanX * 127))
		if midiVal > 127 {
			midiVal = 127
		} else if midiVal < 0 {
			midiVal = 0
		}
		if _, ok := midiOuts[p.MIDIOut]; ok {
			for i := 0; i < 3; i++ {
				if newMidi[handNum*3+i] != lastMidi[handNum*3+i] {
					midiOuts[p.MIDIOut].WriteShort(0xB0, int64(handNum*3+i), newMidi[handNum*3+i])
					lastMidi[handNum*3+i] = newMidi[handNum*3+i]
				}
			}
		}
		mutex.Unlock()
	}
}

func openBrowser(url string) {
	var err error

	switch runtime.GOOS {
	case "linux":
		err = exec.Command("xdg-open", url).Start()
	case "windows":
		// err = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
		err = exec.Command("cmd", "/C", "start", "chrome.exe", url).Run()
	case "darwin":
		err = exec.Command("open", url).Start()
	default:
		err = fmt.Errorf("unsupported platform")
	}
	if err != nil {
		log.Error(err)
	}
	return
}
