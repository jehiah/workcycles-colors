package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/dustin/go-humanize"
	"github.com/gorilla/handlers"
	"github.com/julienschmidt/httprouter"
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

func newTemplate(fs fs.FS, n string) *template.Template {
	funcMap := template.FuncMap{
		"ToLower": strings.ToLower,
		"Comma":   commaInt,
		"Time":    humanize.Time,
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
	w.Header().Add("Cache-Control", fmt.Sprintf("public; max-age=%d", int(duration.Seconds())))
	w.Header().Add("Expires", time.Now().Add(duration).Format(http.TimeFormat))
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
	return err
}

func (a *App) Image(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	img := ps.ByName("img")
	if !strings.HasSuffix(img, ".jpg") {
		http.NotFound(w, r)
		return
	}
	err := a.proxyGoogleStorage(w, r.Context(), fmt.Sprintf("images/%s", img))
	if err == storage.ErrObjectNotExist {
		a.addExpireHeaders(w, time.Minute*10)
		http.NotFound(w, r)
		return
	}
}
func (a *App) BikesJSON(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {

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

func main() {
	logRequests := flag.Bool("log-requests", false, "log requests")
	devMode := flag.Bool("dev-mode", false, "development mode")
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
	router.Handler("GET", "/static/*file", app.staticHandler)

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