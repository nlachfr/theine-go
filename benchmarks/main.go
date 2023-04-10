package main

import (
	"bufio"
	"encoding/binary"
	"encoding/csv"
	"fmt"
	"image/color"
	"io"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Yiling-J/theine-go/benchmarks/clients"
	"golang.org/x/image/font"
	"gonum.org/v1/plot"
	"gonum.org/v1/plot/plotter"
	"gonum.org/v1/plot/vg"
	"gonum.org/v1/plot/vg/draw"
)

const (
	GET = "GET"
	SET = "SET"
)

type key struct {
	key string
	op  string
}

func zipfGen(keyChan chan key) {
	z := rand.NewZipf(rand.New(rand.NewSource(time.Now().UnixNano())), 1.0001, 10, 50000000)
	for i := 0; i < 1000000; i++ {
		keyChan <- key{key: fmt.Sprintf("key:%d", z.Uint64()), op: GET}
	}
	close(keyChan)
}

func ds1Gen(keyChan chan key) {
	f, err := os.Open("trace/ds1")
	if err != nil {
		panic(err)
	}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		s := strings.Split(scanner.Text(), " ")
		base, _ := strconv.Atoi(s[0])
		count, _ := strconv.Atoi(s[1])
		for i := 0; i < count; i++ {
			keyChan <- key{key: strconv.Itoa(base + i), op: GET}
		}
	}
	close(keyChan)
}

func s3Gen(keyChan chan key) {
	f, err := os.Open("trace/s3")
	if err != nil {
		panic(err)
	}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		s := strings.Split(scanner.Text(), " ")
		base, _ := strconv.Atoi(s[0])
		count, _ := strconv.Atoi(s[1])
		for i := 0; i < count; i++ {
			keyChan <- key{key: strconv.Itoa(base + i), op: GET}
		}
	}
	close(keyChan)
}

func scarabGen(keyChan chan key) {
	f, err := os.Open("trace/sc2")
	if err != nil {
		panic(err)
	}
	reader := bufio.NewReader(f)
	for {
		buf := make([]byte, 8)
		_, err := io.ReadFull(reader, buf)
		if err != nil {
			close(keyChan)
			break
		}
		num := binary.BigEndian.Uint64(buf)
		keyChan <- key{key: strconv.Itoa(int(num)), op: GET}
	}

}

func fbGen(keyChan chan key) {
	f, err := os.Open("trace/fb.csv")
	if err != nil {
		panic(err)
	}
	reader := csv.NewReader(f)
	for {
		record, err := reader.Read()
		if err == io.EOF {
			close(keyChan)
			break
		}
		if record[1] == "op" {
			continue
		}
		keyChan <- key{key: record[0], op: record[1]}
	}
}

func infinite(client clients.Client[int, int], cap int, concurrency int) {
	// statsviz.RegisterDefault()

	// go func() {
	// 	log.Println(http.ListenAndServe("localhost:6060", nil))
	// }()
	client.Init(cap)
	var wg sync.WaitGroup
	total := atomic.Uint64{}
	miss := atomic.Uint64{}

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			z := rand.NewZipf(
				rand.New(rand.NewSource(time.Now().UnixNano())), 1.0001, 10, 10000000,
			)
			for {
				total.Add(1)
				_, get := client.GetSet(int(z.Uint64()), 1)
				if !get {
					miss.Add(1)
				}
			}
		}()
	}
	for {
		time.Sleep(2 * time.Second)
		t := total.Load()
		m := miss.Load()
		fmt.Printf("total: %d, hit ratio: %.2f\n", t, float32(t-m)/float32(t))
	}
	wg.Wait()

}

func bench(client clients.Client[string, string], cap int, gen func(keyChan chan key)) float64 {
	counter := 0
	miss := 0
	done := false
	keyChan := make(chan key)
	go gen(keyChan)
	client.Init(cap)
	for !done {
		k, more := <-keyChan
		if more {
			counter++
			if counter%100000 == 0 {
				fmt.Print(".")
			}
			switch k.op {
			case GET:
				v, ok := client.GetSet(k.key, k.key)
				if ok {
					if v != k.key {
						panic("")
					}
				} else {
					miss++
				}
			case SET:
				client.Set(k.key, k.key)
			}
		} else {
			done = true
		}
	}
	hr := float64(counter-miss) / float64(counter)
	fmt.Printf("\n--- %s hit ratio: %.3f\n", client.Name(), hr)
	client.Close()
	time.Sleep(time.Second)
	return hr
}

func benchParallel(client clients.Client[string, string], cap int, gen func(keyChan chan key)) float64 {
	counter := &atomic.Uint32{}
	miss := &atomic.Uint32{}
	keyChan := make(chan key)
	go gen(keyChan)
	client.Init(cap)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			done := false
			for !done {
				k, more := <-keyChan
				if more {
					new := counter.Add(1)
					if new%100000 == 0 {
						fmt.Print(".")
					}
					switch k.op {
					case GET:
						v, ok := client.GetSet(k.key, k.key)
						if ok {
							if v != k.key {
								panic("")
							}
						} else {
							miss.Add(1)
						}
					case SET:
						client.Set(k.key, k.key)
					}
				} else {
					done = true
				}
			}
		}()
	}
	wg.Wait()

	c := counter.Load()
	m := miss.Load()
	hr := float64(c-m) / float64(c)
	fmt.Printf("\n--- %s parallel hit ratio: %.3f\n", client.Name(), hr)
	client.Close()
	time.Sleep(time.Second)
	return hr
}

func benchAndPlot(title string, caps []int, gen func(keyChan chan key), parallel bool) {
	p := plot.New()
	p.Title.Text = fmt.Sprintf("Hit Ratios - %s", title)
	p.X.Label.Text = "capacity"
	p.Y.Label.Text = "hit ratio"
	p.Legend.TextStyle.Font.Size = vg.Points(16)
	p.Legend.TextStyle.Font.Style = font.StyleOblique
	p.Title.TextStyle.Font.Size = vg.Points(16)
	p.Title.TextStyle.Font.Style = font.StyleOblique
	p.X.Label.TextStyle.Font.Size = vg.Points(14)
	p.Y.Label.TextStyle.Font.Size = vg.Points(14)

	tdata := plotter.XYs{}
	rdata := plotter.XYs{}
	ldata := plotter.XYs{}
	for _, cap := range caps {
		tdot := plotter.XY{X: float64(cap)}
		rdot := plotter.XY{X: float64(cap)}
		ldot := plotter.XY{X: float64(cap)}
		fmt.Printf("======= %s cache size: %d =======\n", strings.ToLower(title), cap)
		bencher := bench
		if parallel {
			bencher = benchParallel
		}
		tdot.Y = bencher(&clients.Theine[string, string]{}, cap, gen)
		rdot.Y = bencher(&clients.Ristretto[string, string]{}, cap, gen)
		ldot.Y = bencher(&clients.LRU[string, string]{}, cap, gen)
		tdata = append(tdata, tdot)
		rdata = append(rdata, rdot)
		ldata = append(ldata, ldot)
	}
	tline, tpoints, err := plotter.NewLinePoints(tdata)
	if err != nil {
		panic(err)
	}
	tline.Color = color.RGBA{B: 255, A: 255}
	tpoints.Shape = draw.BoxGlyph{}
	rline, rpoints, err := plotter.NewLinePoints(rdata)
	if err != nil {
		panic(err)
	}
	rline.Color = color.RGBA{G: 255, A: 255}
	rpoints.Shape = draw.CircleGlyph{}
	lline, lpoints, err := plotter.NewLinePoints(ldata)
	if err != nil {
		panic(err)
	}
	lline.Color = color.RGBA{R: 255, A: 255}
	lpoints.Shape = draw.PyramidGlyph{}
	p.Add(tline, tpoints, rline, rpoints, lline, lpoints)
	p.Legend.Add("theine", tline, tpoints)
	p.Legend.Add("ristretto", rline, rpoints)
	p.Legend.Add("lru", lline, lpoints)
	if parallel {
		title += "-parallel"
	}
	if err := p.Save(
		16*vg.Inch, 9*vg.Inch, fmt.Sprintf("results/%s.png", strings.ToLower(title)),
	); err != nil {
		panic(err)
	}

}

func main() {
	// infinite(&clients.Theine[int, int]{}, 100000, 12)
	// infinite(&clients.Ristretto[int, int]{}, 100000, 12)

	parallel := false
	benchAndPlot("Zipf", []int{100, 200, 500, 1000, 2000, 5000, 10000, 20000}, zipfGen, parallel)
	benchAndPlot("DS1", []int{1000000, 2000000, 3000000, 5000000, 6000000, 8000000}, ds1Gen, parallel)
	benchAndPlot("S3", []int{50000, 100000, 200000, 300000, 500000, 800000, 1000000}, s3Gen, parallel)
	benchAndPlot("SCARAB1H", []int{1000, 2000, 5000, 10000, 20000, 50000, 100000}, scarabGen, parallel)
	benchAndPlot("META", []int{10000, 20000, 50000, 80000, 100000}, fbGen, parallel)
}
