package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oschwald/geoip2-golang"

	"github.com/ulule/limiter/v3"
	"github.com/ulule/limiter/v3/drivers/middleware/stdlib"
	sim "github.com/ulule/limiter/v3/drivers/store/memory"

	"golang.org/x/sync/errgroup"
)

const (
	topStories           = "https://hacker-news.firebaseio.com/v0/topstories.json"
	bestStories          = "https://hacker-news.firebaseio.com/v0/beststories.json"
	storyLink            = "https://hacker-news.firebaseio.com/v0/item/%d.json"
	hnPostLink           = "https://news.ycombinator.com/item?id=%d"
	frontPageNumArticles = 30
	hnPollTime           = 1 * time.Minute
	defaultPort          = 8080

	rateLimit          = "5-M"
	eightHrs           = 8 * 60 * 60
	humanTrackingLimit = 300
	itemFromFile       = "file"
	itemFromHN         = "hn"

	maxMindDatabseURL = "http://geolite.maxmind.com/download/geoip/database/GeoLite2-City.mmdb.gz"
)

type changeAction string

const (
	changeAdd    changeAction = "added"
	changeRemove              = "removed"
)

type logLevel string

const (
	logInfo    logLevel = "info"
	logWarning          = "warning"
	logDebug            = "debug"
	logFatal            = "fatal"
)

type appErr struct {
	err   error
	level logLevel
}

func (ae appErr) Error() string {
	return fmt.Sprintf("occured: %v with level: %s", ae.err, ae.level)
}

type itemList []int

type unixTime int64

type change struct {
	Action changeAction
	Item   *item
}

func (c change) String() string {
	return fmt.Sprintf("%s : %d", c.Action, c.Item.ID)
}

type adder struct {
	sync.Mutex
	count int
}

var version string

func main() {
	var port int
	// use PORT env else port
	envPort := os.Getenv("PORT")
	if envPort == "" {
		port = defaultPort
	} else {
		var err error
		port, err = strconv.Atoi(envPort)
		if err != nil {
			panic(err)
		}
	}

	templateFile := os.Getenv("TMPL_PATH")
	if templateFile == "" {
		templateFile = "./index.html"
	}

	inputFilePath := os.Getenv("INPUT_PATH")
	if inputFilePath == "" {
		inputFilePath = "./input.json"
	}

	outputFilePath := os.Getenv("OUTPUT_PATH")
	if outputFilePath == "" {
		outputFilePath = "./output.json"
	}

	tmpl := template.New("index.html")
	tmpl, err := tmpl.ParseFiles(templateFile)
	if err != nil {
		panic(err)
	}

	store := sim.NewStore()
	// Define a limit rate to 5 requests per minute.
	rate, err := limiter.NewRateFromFormatted(rateLimit)
	if err != nil {
		panic(err)
	}

	fileDownloadCount := 0
	var serverETag string
	resp, err := http.Head(maxMindDatabseURL)
	if err != nil {
		panic(err)
	}
	serverETag = resp.Header.Get("ETag")

	mmdbPath := "GeoLite2-City.mmdb"

	appCtx, cancel := context.WithCancel(context.Background())

	geoIPReader, err := geoip2.Open(mmdbPath)
	if err != nil {
		panic(err)
	}

	queue := &limitQueue{
		limit: humanTrackingLimit,
		keys:  []int{},
		store: make(map[int]*item),
	}

	app := app{
		geoIPReader: geoIPReader,
		lq:          queue,
	}

	var newFilePath string
	var ETag string

	fetchLatestMmdb := func(ctx context.Context) (*geoip2.Reader, error) {
		newFilePath = fmt.Sprintf("GeoLite2-City-%d.mmdb", fileDownloadCount)
		req, err := http.NewRequest(http.MethodHead, maxMindDatabseURL, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}

		ETag = resp.Header.Get("Last-Modified")
		lastModified, err := time.Parse(http.TimeFormat, resp.Header.Get("Last-Modified"))
		if err != nil {
			return nil, err
		}

		if ETag != serverETag || lastModified.After(time.Now()) {
			out, err := os.Create(newFilePath)
			if err != nil {
				return nil, err
			}
			defer out.Close()
			req, err := http.NewRequest(http.MethodGet, maxMindDatabseURL, nil)
			if err != nil {
				return nil, err
			}
			resp, err := http.DefaultClient.Do(req.WithContext(ctx))
			if err != nil {
				return nil, err
			}

			defer resp.Body.Close()

			unziper, err := gzip.NewReader(resp.Body)
			if err != nil {
				return nil, err
			}

			defer unziper.Close()

			_, err = io.Copy(out, unziper)
			if err != nil {
				return nil, err
			}

			fileDownloadCount++

			return geoip2.Open(newFilePath)
		}

		return nil, fmt.Errorf("ETag != serverETag || lastModified.After(time.Now()) is not true")
	}

	twentyFourHrs := 24 * time.Hour
	mmdbDownloader := func(ctx context.Context) {
		for range time.Tick(twentyFourHrs) {
			geoIPReader, err := fetchLatestMmdb(ctx)
			if err != nil {
				log.Println(err)
				continue
			}
			app.Lock()
			app.geoIPReader = geoIPReader
			app.Unlock()

			err = os.Remove(mmdbPath)
			if err != nil {
				log.Println(err)
				continue
			}
			mmdbPath = newFilePath
			serverETag = ETag
		}
	}

	middleware := stdlib.NewMiddleware(limiter.New(store, rate, limiter.WithTrustForwardHeader(true)))

	errCh := make(chan appErr)
	changeCh := make(chan change)

	incomingItems := make(chan *item)

	r := strings.NewReplacer("http://", "", "https://", "", "www.", "", "www2.", "", "www3.", "")
	urlToDomain := func(link string) (string, error) {
		u, err := url.Parse(link)
		if err != nil {
			return "", err
		}
		parts := strings.Split(u.Hostname(), ".")
		if len(parts) >= 2 {
			domain := parts[len(parts)-2] + "." + parts[len(parts)-1]
			return domain, nil
		}

		return r.Replace(u.Hostname()), nil
	}

	fetchTopStories := func(ctx context.Context, limit int) ([]int, error) {
		req, err := http.NewRequest(http.MethodGet, topStories, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req.WithContext(ctx))
		if err != nil {
			return nil, err
		}

		defer resp.Body.Close()

		decoder := json.NewDecoder(resp.Body)
		var items itemList
		err = decoder.Decode(&items)
		if err != nil {
			return nil, err
		}
		if len(items) < limit {
			limit = len(items)
		}

		return items[:limit], nil
	}

	fetchStoriesFromFile := func(inputFilePath string) ([]*item, error) {
		f, err := os.Open(inputFilePath)
		if err != nil {
			return nil, err
		}
		defer f.Close()

		dec := json.NewDecoder(f)
		var items []*item
		err = dec.Decode(&items)
		if err != nil {
			return nil, err
		}
		return items, nil
	}

	serverInputsToUserFetcher := func(ctx context.Context, inputFilePath string) error {
		items, err := fetchStoriesFromFile(inputFilePath)
		if err != nil {
			return err
		}
		for _, it := range items {
			if it.From == "" {
				it.From = itemFromFile
			}

			incomingItems <- it
		}

		return nil
	}

	fetchItem := func(ctx context.Context, itemID int) (*item, error) {
		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf(storyLink, itemID), nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req.WithContext(ctx))
		if err != nil {
			return nil, err
		}

		defer resp.Body.Close()

		decoder := json.NewDecoder(resp.Body)
		var it item
		err = decoder.Decode(&it)
		if err != nil {
			return nil, err
		}

		return &it, nil
	}

	topStoriesFetcher := func(ctx context.Context, limit int) error {
		// send items
		itemIds, err := fetchTopStories(ctx, limit)
		if err != nil {
			return err
		}
		for _, itID := range itemIds {
			if appCtx.Err() != nil {
				return nil
			}

			item, err := fetchItem(ctx, itID)
			if err != nil {
				log.Println(err) // warning
			} else {
				incomingItems <- item
			}
		}

		return nil
	}

	storyLister := func(ctx context.Context) {
		for item := range incomingItems {
			func() {
				app.Lock()
				defer app.Unlock()

				if item.Added == 0 {
					item.Added = unixTime(time.Now().Unix())
				}
				if item.Domain == "" {
					domain, err := urlToDomain(item.URL)
					if err == nil {
						item.Domain = domain
					} else {
						log.Println(err)
					}
				}
				if item.From != itemFromFile { // from file
					item.DiscussLink = fmt.Sprintf(hnPostLink, item.ID)
				}

				removedItemIfAny := app.lq.add(item)

				if removedItemIfAny != nil {
					changeCh <- change{
						Action: changeRemove,
						Item:   removedItemIfAny,
					}
				}

				changeCh <- change{
					Action: changeAdd,
					Item:   item,
				}

			}()
		}
	}

	storyRemover := func(ctx context.Context) error {
		topItems, err := fetchTopStories(ctx, frontPageNumArticles)
		if err != nil {
			return err
		}

		fileItems, err := fetchStoriesFromFile(inputFilePath)
		if err != nil {
			return err
		}

		app.Lock()
		defer app.Unlock()
		for id, it := range app.lq.store {
			if ctx.Err() != nil {
				return nil
			}

			stillAtTop := false
			switch it.From {
			case itemFromFile:
				for _, it := range fileItems {
					if id == it.ID {
						stillAtTop = true
						break
					}
				}

			default:
				for _, tid := range topItems {
					if tid == id {
						stillAtTop = true
						break
					}
				}
			}

			if !stillAtTop && time.Since(time.Unix(int64(it.Added), 0)).Seconds() > eightHrs {
				// send these to removed chan
				changeCh <- change{
					Action: changeRemove,
					Item:   app.lq.store[id],
				}
				app.lq.remove(id)
			}
		}
		return nil
	}

	changeLogger := func() {
		for c := range changeCh {
			log.Println(c)
		}
	}

	errLogger := func() {
		for err := range errCh {
			log.Println(err)
		}
	}

	const headerXForwardedFor = "X-Forwarded-For"
	const headerXRealIP = "X-Real-IP"
	realIP := func(r *http.Request) string {
		ra := r.RemoteAddr
		if ip := r.Header.Get(headerXForwardedFor); ip != "" {
			ra = strings.Split(ip, ", ")[0]
		} else if ip := r.Header.Get(headerXRealIP); ip != "" {
			ra = ip
		} else {
			ra, _, _ = net.SplitHostPort(ra)
		}

		return ra
	}

	var visitCount adder

	http.Handle("/", middleware.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		possibleIP := realIP(r)
		log.Println(possibleIP)
		log.Println(r.UserAgent())

		ip := net.ParseIP(possibleIP)

		app.Lock()
		defer app.Unlock()

		city, err := app.geoIPReader.City(ip)
		if err != nil {
			log.Println(err)
		} else {
			log.Println(city.Country.IsoCode)
		}

		data := make(map[string]interface{})
		data["Data"] = app.lq.store

		visitCount.Lock()
		defer visitCount.Unlock()

		visitCount.count++

		data["Visits"] = visitCount.count
		data["Version"] = version

		err = tmpl.Execute(w, data)
		if err != nil {
			errCh <- appErr{
				err:   err,
				level: logFatal,
			}
		}
		return
	})))

	log.Println("START")
	log.Println("starting the app")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, os.Kill)

	intervalTicker := time.NewTicker(hnPollTime)

	go func() {
		for range intervalTicker.C {
			log.Println("starting ticker ticker")
			eg, ctx := errgroup.WithContext(appCtx)
			eg.Go(func() error {
				log.Println("starting top stories fetcher")
				return topStoriesFetcher(ctx, frontPageNumArticles)
			})
			eg.Go(func() error {
				log.Println("starting file stories fetcher")
				return serverInputsToUserFetcher(ctx, inputFilePath)
			})
			eg.Go(func() error {
				log.Println("starting story remover")
				return storyRemover(ctx)
			})
			err := eg.Wait()
			if err != nil {
				errCh <- appErr{
					err:   err,
					level: logWarning,
				}
			}
		}
	}()

	log.Println("starting error logger")
	go errLogger()

	log.Println("starting change logger")
	go changeLogger()

	log.Println("starting top stories fetcher")
	go topStoriesFetcher(appCtx, frontPageNumArticles)

	log.Println("starting mmdb downloader")
	go mmdbDownloader(appCtx)

	go func() {
		err := serverInputsToUserFetcher(appCtx, inputFilePath)
		if err != nil {
			log.Println(err)
		}
	}()

	log.Println("starting story lister")
	go storyLister(appCtx)

	srv := &http.Server{Addr: fmt.Sprintf(":%d", port)}

	go func() {
		log.Println(srv.ListenAndServe())
	}()
	sig := <-stop
	log.Printf("interrupted with signal %s, aborting\n", sig.String())

	ctx, c := context.WithTimeout(appCtx, 2*time.Second)
	defer c()
	err = srv.Shutdown(ctx)
	if err != nil {
		log.Println(err)
	}

	cleanup := func() {
		cancel()
		intervalTicker.Stop()
		close(incomingItems)
		close(changeCh)
		close(errCh)
	}

	log.Println("clean up")
	cleanup()
	log.Println("clean up done")

	// dump stories at the end
	stories, _ := json.Marshal(app.lq.store)
	log.Println(ioutil.WriteFile(outputFilePath, stories, 0644))

	log.Println("END")
}
