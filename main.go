package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/gorilla/websocket"
	"github.com/microcosm-cc/bluemonday"
	log "github.com/schollz/logger"
	bolt "go.etcd.io/bbolt"
)

// flag for port
var flagPort string
var flagLog string
var flagMigrate string
var db *bolt.DB

//go:embed static/*
var content embed.FS
var policy *bluemonday.Policy

func init() {
	flag.StringVar(&flagPort, "port", "8001", "port to run the server on")
	flag.StringVar(&flagLog, "log", "info", "log level")
	flag.StringVar(&flagMigrate, "migrate", "", "migrate from old cowyo")
}

func main() {
	flag.Parse()
	log.SetLevel(flagLog)

	policy = bluemonday.UGCPolicy()

	// databse is bolt db database
	var err error
	db, err = bolt.Open("cowyo2.db", 0600, nil)
	if err != nil {
		log.Error(err)
	}

	// create cowyo bucket if it doesn't exist
	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte("cowyo"))
		if err != nil {
			return fmt.Errorf("create bucket: %s", err)
		}
		return nil
	})
	if err != nil {
		log.Error(err)
	}

	if flagMigrate != "" {
		log.Infof("migrating from %s", flagMigrate)
		migrate()
	}
	// start server
	Serve()
}

type OldCowyo struct {
	Name string `json:"Name"`
	Text struct {
		CurrentText string `json:"CurrentText"`
	} `json:"Text"`
	RenderedPage            string `json:"RenderedPage"`
	IsLocked                bool   `json:"IsLocked"`
	PassphraseToUnlock      string `json:"PassphraseToUnlock"`
	IsEncrypted             bool   `json:"IsEncrypted"`
	IsPrimedForSelfDestruct bool   `json:"IsPrimedForSelfDestruct"`
}

func migrate() (err error) {
	var folder = flagMigrate
	// find all the .json files in folder
	files, err := filepath.Glob(filepath.Join(folder, "*.json"))
	if err != nil {
		log.Error(err)
		return
	}

	// iterate through all the files
	for _, file := range files {
		log.Infof("migrating %s", file)
		// read the file
		var b []byte
		b, err = os.ReadFile(file)
		if err != nil {
			log.Error(err)
			continue
		}
		var old OldCowyo
		err = json.Unmarshal(b, &old)
		if err != nil {
			log.Error(err)
			continue
		}

		err = db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte("cowyo"))
			if b == nil {
				return nil
			}
			var p Page
			p.Title = old.Name
			p.Text = old.Text.CurrentText
			by, _ := json.Marshal(p)
			return b.Put([]byte(p.Title), []byte(by))
		})
		if err != nil {
			log.Error(err)
			continue
		}
	}

	db.Close()
	os.Exit(0)
	return
}

var indexTemplate *template.Template

func Serve() {
	// load go template from index.html
	// parse template from embed
	indexTemplate = template.Must(template.ParseFS(content, "static/index.html"))

	log.Infof("listening on :%s", flagPort)
	http.HandleFunc("/", handler)
	http.ListenAndServe(fmt.Sprintf(":%s", flagPort), nil)
}

func handler(w http.ResponseWriter, r *http.Request) {
	t := time.Now().UTC()
	// Redirect URLs with trailing slashes (except for the root "/")
	if r.URL.Path != "/" && strings.HasSuffix(r.URL.Path, "/") {
		http.Redirect(w, r, strings.TrimRight(r.URL.Path, "/"), http.StatusPermanentRedirect)
		return
	}
	err := handle(w, r)
	if err != nil {
		log.Error(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	log.Infof("%v %v %v %s\n", r.RemoteAddr, r.Method, r.URL.Path, time.Since(t))
}

type Page struct {
	Title       string `json:"title"`
	Text        string `json:"text"`
	CursorStart int    `json:"cursor_start"`
	CursorEnd   int    `json:"cursor_end"`
}

func handle(w http.ResponseWriter, r *http.Request) (err error) {
	if r.URL.Path == "/ws" {
		return handleWebsocket(w, r)
	} else if strings.HasPrefix(r.URL.Path, "/static") {
		// serve static file from embed
		w.Header().Set("Cache-Control", "max-age=86400")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Cache-Control", "must-revalidate")
		w.Header().Set("Cache-Control", "proxy-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		http.FileServer(http.FS(content)).ServeHTTP(w, r)
		return
	} else if r.URL.Path == "/" {
		// generate random number between 1000 and 9999
		number := rand.Intn(8999) + 1000
		http.Redirect(w, r, "/"+fmt.Sprint(number), 302)
		return
	}
	key := r.URL.Path[1:]
	p := Page{Title: key}
	// load from database
	err = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("cowyo"))
		if b == nil {
			return nil
		}
		v := b.Get([]byte(key))
		if v == nil {
			return nil
		} else {
			err = json.Unmarshal(v, &p)
			if err != nil {
				log.Error(err)
			}
		}
		return nil
	})
	log.Tracef("loading: %+v", p)
	p.Text = policy.Sanitize(p.Text)
	return indexTemplate.Execute(w, p)
}

var upgrader = websocket.Upgrader{} // use default options

func handleWebsocket(w http.ResponseWriter, r *http.Request) (err error) {
	// get the place from the query parameter
	query := r.URL.Query()
	log.Tracef("query: %+v", query)
	if _, ok := query["place"]; !ok {
		err = fmt.Errorf("no place")
		log.Error(err)
		return
	}
	place := query["place"][0]

	// use gorilla to open websocket
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	for {
		var p Page
		err := c.ReadJSON(&p)
		if err != nil {
			break
		}
		log.Tracef("received %+v", p)
		log.Tracef("updating '%s' with %d bytes", place, len(p.Text))
		// update text for the current place
		err = db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte("cowyo"))
			if b == nil {
				return nil
			}
			p.Title = place
			by, _ := json.Marshal(p)
			return b.Put([]byte(place), []byte(by))
		})
		if err != nil {
			log.Error(err)
		}
		c.WriteJSON(Page{Title: "ok"})
	}
	return
}
