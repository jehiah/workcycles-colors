package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"cloud.google.com/go/storage"
	"github.com/dustin/go-humanize"
	"github.com/google/uuid"
	"github.com/gorilla/handlers"
	"github.com/gorilla/schema"
	"github.com/julienschmidt/httprouter"
	"google.golang.org/api/iterator"
	"gopkg.in/yaml.v3"
)

//go:embed templates/*
var content embed.FS

//go:embed static/*
var static embed.FS

var americaNewYork, _ = time.LoadLocation("America/New_York")

type App struct {
	gsclient *storage.Client
	devMode  bool

	staticHandler http.Handler
	templateFS    fs.FS
}

func commaInt(i int) string {
	return humanize.Comma(int64(i))
}
func yamlize(i interface{}) (string, error) {
	var b bytes.Buffer
	e := yaml.NewEncoder(&b)
	e.SetIndent(2)
	err := e.Encode(i)
	return b.String(), err
}

func newTemplate(fs fs.FS, n string) *template.Template {
	funcMap := template.FuncMap{
		"ToLower": strings.ToLower,
		"Comma":   commaInt,
		"Time":    humanize.Time,
		"yaml":    yamlize,
	}
	t := template.New("empty").Funcs(funcMap)
	return template.Must(t.ParseFS(fs, filepath.Join("templates", n), "templates/base.html"))
}

// RobotsTXT renders /robots.txt
func (a *App) RobotsTXT(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	w.Header().Set("content-type", "text/plain")
	a.addExpireHeaders(w, time.Hour*24*7)
	io.WriteString(w, "# robots welcome\n# https://github.com/jehiah/workcycles-colors\n")
}

func (a *App) addExpireHeaders(w http.ResponseWriter, duration time.Duration) {
	if a.devMode {
		return
	}
	if w.Header().Get("Cache-Control") != "" {
		return
	}
	w.Header().Set("Cache-Control", fmt.Sprintf("public; max-age=%d", int(duration.Seconds())))
	w.Header().Set("Expires", time.Now().Add(duration).Format(http.TimeFormat))
}

func clearExpireHeaders(w http.ResponseWriter) {
	w.Header().Del("Cache-Control")
	w.Header().Del("Expires")
}

func (a *App) proxyGoogleStorage(w http.ResponseWriter, ctx context.Context, filename string) error {
	r, err := a.gsclient.Bucket("workcycles-colors").Object(filename).NewReader(ctx)
	if err != nil {
		return err
	}
	defer r.Close()
	a.addExpireHeaders(w, time.Hour)
	if t := mime.TypeByExtension(filepath.Ext(filename)); t != "" {
		w.Header().Add("Content-Type", t)
	}
	_, err = io.Copy(w, r)
	if err != nil {
		clearExpireHeaders(w)
	}
	return err
}

func (a *App) Image(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	a.ProxyImage(w, r, ps, "images/")
}
func (a *App) UploadImage(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	a.ProxyImage(w, r, ps, "uploaded/")
}
func (a *App) ProxyImage(w http.ResponseWriter, r *http.Request, ps httprouter.Params, prefix string) {
	img := ps.ByName("img")
	switch filepath.Ext(img) {
	case ".jpg", ".png":
	default:
		http.NotFound(w, r)
		return
	}
	a.addExpireHeaders(w, time.Hour*6)
	err := a.proxyGoogleStorage(w, r.Context(), filepath.Join(prefix, img))
	if err == storage.ErrObjectNotExist {
		a.addExpireHeaders(w, time.Minute*10)
		http.NotFound(w, r)
		return
	}
}

func (a *App) Index(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	t := newTemplate(a.templateFS, "index.html")
	w.Header().Set("content-type", "text/html")
	err := t.ExecuteTemplate(w, "index.html", nil)
	if err != nil {
		log.Print(err)
		http.Error(w, "Internal Server Error", 500)
	}
}

var decoder = schema.NewDecoder()

type Photo struct {
	Copyright string
	Bike      string // i.e. Fr8, Kr8
	Colors    string
	SRC       string // url to source
	ImageURL  string
	Time      time.Time
}

func (p Photo) MarshalYAML() (interface{}, error) {
	f := func(c rune) bool {
		return !unicode.IsLetter(c) && !unicode.IsNumber(c)
	}
	return struct {
		Photo interface{} `yaml:"photo"`
	}{
		Photo: struct {
			Copyright string    `yaml:"copyright"`
			Bike      string    `yaml:"bike"`
			SRC       string    `yaml:"src"`
			Time      time.Time `yaml:"added"`
			Image     string    `yaml:"image"`
			Color     []string  `yaml:"color"`
		}{
			Copyright: p.Copyright,
			Bike:      p.Bike,
			SRC:       p.SRC,
			Time:      p.Time.Truncate(time.Minute),
			Image:     "images/" + p.ImageURL,
			Color:     strings.FieldsFunc(p.Colors, f),
		},
	}, nil
}

func (p Photo) Validate() error {
	if strings.TrimSpace(p.Copyright) == "" {
		return errors.New(`required field "Copyright" missing`)
	}
	if strings.TrimSpace(p.Colors) == "" {
		return errors.New(`required field "Colors" missing`)
	}
	if strings.TrimSpace(p.SRC) != "" {
		_, err := url.Parse(strings.TrimSpace(p.SRC))
		if err != nil {
			return errors.New(`field "Link" is invalid URL`)
		}
	}
	return nil
}

func (a *App) Upload(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	t := newTemplate(a.templateFS, "upload.html")
	w.Header().Set("content-type", "text/html")
	err := t.ExecuteTemplate(w, "upload.html", nil)
	if err != nil {
		log.Print(err)
		http.Error(w, "Internal Server Error", 500)
	}
}
func (a *App) UploadPost(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	if err := r.ParseMultipartForm(1024 * 1024 * 15); err != nil {
		log.Print(err)
		http.Error(w, "Internal Server Error", 500)
		return
	}

	var p Photo
	if err := decoder.Decode(&p, r.MultipartForm.Value); err != nil {
		log.Print(err)
		http.Error(w, "Internal Server Error", 500)
		return
	}

	if err := p.Validate(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	f, h, err := r.FormFile("img")
	if err != nil {
		log.Print(err)
		http.Error(w, "Image not found", 400)
		return
	}

	filename := uuid.NewString()
	metadata := filename + ".json"
	switch h.Header.Get("Content-Type") {
	case "image/jpeg":
		filename += ".jpg"
	case "image/png":
		filename += ".png"
	}

	log.Printf("uploading to gs://workcycles-colors/uploaded/%s", filename)
	fw := a.gsclient.Bucket("workcycles-colors").Object("uploaded/" + filename).NewWriter(r.Context())
	_, err = io.Copy(fw, f)
	if err != nil {
		log.Print(err)
		http.Error(w, "Internal Server Error", 400)
		return
	}
	err = fw.Close()
	if err != nil {
		log.Print(err)
		http.Error(w, "Internal Server Error", 400)
		return
	}
	log.Printf("%#v", h.Header)

	fw = a.gsclient.Bucket("workcycles-colors").Object("uploaded/" + metadata).NewWriter(r.Context())
	p.Time = time.Now().UTC()
	p.ImageURL = filename
	err = json.NewEncoder(fw).Encode(p)
	if err != nil {
		log.Print(err)
		http.Error(w, "Internal Server Error", 400)
		return
	}
	err = fw.Close()
	if err != nil {
		log.Print(err)
		http.Error(w, "Internal Server Error", 400)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte("Thank You.\n\nYour upload will be reviewed in 1-2 days."))
}

func (a *App) Admin(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	bucket := a.gsclient.Bucket("workcycles-colors")
	query := &storage.Query{Prefix: "uploaded/"}

	var files []string
	it := bucket.Objects(r.Context(), query)
	for {
		attrs, err := it.Next()
		if err == iterator.Done || len(files) > 50 {
			break
		}
		if err != nil {
			log.Printf("%s", err)
			http.Error(w, "Unknown Error", 500)
			return
		}
		if !strings.HasSuffix(attrs.Name, ".json") {
			continue
		}
		files = append(files, attrs.Name)
	}
	var photos []Photo
	for _, f := range files {
		r, err := bucket.Object(f).NewReader(r.Context())
		if err != nil {
			log.Printf("%s", err)
			http.Error(w, "Unknown Error", 500)
			return
		}
		var p Photo
		err = json.NewDecoder(r).Decode(&p)
		if err != nil {
			log.Printf("%s", err)
			http.Error(w, "Unknown Error", 500)
			return
		}
		photos = append(photos, p)
	}

	t := newTemplate(a.templateFS, "admin.html")
	w.Header().Set("content-type", "text/html")
	err := t.ExecuteTemplate(w, "admin.html", struct {
		Photos []Photo
	}{
		Photos: photos,
	})
	if err != nil {
		log.Printf("%s", err)
		http.Error(w, "Unknown Error", 500)
	}
}

func (a *App) AdminPost(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	r.ParseForm()
	img := r.PostForm.Get("image_file")

	bucket := a.gsclient.Bucket("workcycles-colors")
	src := bucket.Object("uploaded/" + img)
	dst := bucket.Object("images/" + img)
	if _, err := dst.CopierFrom(src).Run(r.Context()); err != nil {
		log.Printf("%s", err)
		http.Error(w, "Unknown Error", 500)
		return
	}
	if err := src.Delete(r.Context()); err != nil {
		log.Printf("%s", err)
		http.Error(w, "Unknown Error", 500)
		return
	}
	if err := bucket.Object("uploaded/" + strings.TrimSuffix(img, filepath.Ext(img)) + ".json").Delete(r.Context()); err != nil {
		log.Printf("%s", err)
		http.Error(w, "Unknown Error", 500)
		return
	}
	http.Redirect(w, r, "/_admin/", 302)
}

func main() {
	logRequests := flag.Bool("log-requests", false, "log requests")
	devMode := flag.Bool("dev-mode", false, "development mode")
	enableAdmin := flag.Bool("enable-admin", false, "enable admin UI")
	flag.Parse()

	client, err := storage.NewClient(context.Background())
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}
	defer client.Close()

	log.Print("starting server...")

	app := &App{
		gsclient:      client,
		devMode:       *devMode,
		staticHandler: http.FileServer(http.FS(static)),
		templateFS:    content,
	}
	if *devMode {
		app.templateFS = os.DirFS(".")
		app.staticHandler = http.StripPrefix("/static/", http.FileServer(http.Dir("static")))
	}

	router := httprouter.New()
	router.GET("/", app.Index)
	router.GET("/images/:img", app.Image)
	router.GET("/robots.txt", app.RobotsTXT)
	router.GET("/upload", app.Upload)
	router.POST("/upload", app.UploadPost)
	router.Handler("GET", "/static/*file", app.staticHandler)

	if *enableAdmin {
		router.GET("/_admin/", app.Admin)
		router.POST("/_admin/", app.AdminPost)
		router.GET("/_admin/images/:img", app.UploadImage)
	}

	// Determine port for HTTP service.
	port := os.Getenv("PORT")
	if port == "" {
		port = "8082"
	}

	var h http.Handler = router
	if *logRequests {
		h = handlers.LoggingHandler(os.Stdout, h)
	}

	// Start HTTP server.
	log.Printf("listening on port %s", port)
	if err := http.ListenAndServe(":"+port, h); err != nil {
		log.Fatal(err)
	}
}
