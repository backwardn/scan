package main

import (
	"database/sql"
	"flag"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/labstack/echo"
	"github.com/labstack/echo/middleware"
	_ "github.com/mattn/go-sqlite3"
)

var dbFile = "scan.db"

type port struct {
	Port    int    `json:"port"`
	Proto   string `json:"proto"`
	Status  string `json:"status"`
	Service struct {
		Name   string `json:"name"`
		Banner string `json:"banner"`
	} `json:"service"`
}

// Results posted from masscan
type result struct {
	IP    string `json:"ip"`
	Ports []port `json:"ports"`
}

// Data retrieved from the database for display
type scandata struct {
	IP        string
	Port      int
	Proto     string
	FirstSeen string
	LastSeen  string
	New       bool
}

// Load all data for displaying in the browser
func load(s string) ([]scandata, error) {
	db, err := sql.Open("sqlite3", dbFile)
	if err != nil {
		return []scandata{}, err
	}
	defer db.Close()

	var where string
	if s != "" {
		where = `WHERE ip LIKE ?`
		s = fmt.Sprintf("%%%s%%", s)
	}

	qry := fmt.Sprintf(`SELECT ip, port, proto, firstseen, lastseen FROM scan %s ORDER BY port, proto, ip, lastseen`, where)
	rows, err := db.Query(qry, s)
	if err != nil {
		return []scandata{}, err
	}

	defer rows.Close()

	var data []scandata
	var ip, proto, firstseen, lastseen string
	var port int

	for rows.Next() {
		err := rows.Scan(&ip, &port, &proto, &firstseen, &lastseen)
		if err != nil {
			return []scandata{}, err
		}
		f, _ := time.Parse("2006-01-02 15:04", firstseen)
		l, _ := time.Parse("2006-01-02 15:04", lastseen)
		data = append(data, scandata{ip, port, proto, firstseen, lastseen, l.Equal(f)})
	}

	return data, nil
}

// Save the results posted
func save(results []result) error {
	db, err := sql.Open("sqlite3", dbFile)
	if err != nil {
		return err
	}
	defer db.Close()

	txn, err := db.Begin()
	if err != nil {
		return err
	}

	insert, err := txn.Prepare(`INSERT INTO scan (ip, port, proto, firstseen, lastseen) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		txn.Rollback()
		return err
	}
	qry, err := db.Prepare(`SELECT 1 FROM scan WHERE ip=? AND port=? AND proto=?`)
	if err != nil {
		txn.Rollback()
		return err
	}
	update, err := txn.Prepare(`UPDATE scan SET lastseen=? WHERE ip=? AND port=? AND proto=?`)
	if err != nil {
		txn.Rollback()
		return err
	}

	now := time.Now()
	nowString := now.Format("2006-01-02 15:04")

	for _, r := range results {
		// Although it's an array, only one port is in each
		port := r.Ports[0]

		// Search for the IP/port/proto combo
		// If it exists, update `lastseen`, else insert a new record

		// Because we have to scan into something
		var x int
		err := qry.QueryRow(r.IP, port.Port, port.Proto).Scan(&x)
		switch {
		case err == sql.ErrNoRows:
			_, err = insert.Exec(r.IP, port.Port, port.Proto, nowString, nowString)
			if err != nil {
				txn.Rollback()
				return err
			}
			continue
		case err != nil:
			txn.Rollback()
			return err
		}

		_, err = update.Exec(nowString, r.IP, port.Port, port.Proto)
		if err != nil {
			txn.Rollback()
			return err
		}
	}

	txn.Commit()
	return nil
}

// Template is a template
type Template struct {
	templates *template.Template
}

// Render renders template
func (t *Template) Render(w io.Writer, name string, data interface{}, c echo.Context) error {
	return t.templates.ExecuteTemplate(w, name, data)
}

type indexData struct {
	Total   int
	Latest  int
	Results []scandata
}

// Handler for GET /
func index(c echo.Context) error {
	ip := c.QueryParam("ip")
	results, err := load(ip)
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
	}

	data := indexData{Results: results, Total: len(results)}

	timeFmt := "2006-01-02 15:04"

	// Find all the latest results and store the number in the struct
	var latest time.Time
	for _, r := range results {
		last, _ := time.Parse(timeFmt, r.LastSeen)
		if last.After(latest) {
			latest = last
		}
	}
	for _, r := range results {
		last, _ := time.Parse(timeFmt, r.LastSeen)
		if last.Equal(latest) {
			data.Latest++
		}
	}

	return c.Render(http.StatusOK, "index", data)
}

// Handler for GET /ips.json
// This is used as the prefetch for Typeahead.js
func ips(c echo.Context) error {
	data, err := load("")
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
	}
	var ips []string
	for _, r := range data {
		ips = append(ips, r.IP)
	}
	return c.JSON(http.StatusOK, ips)
}

// Handler for POST /results
func recvResults(c echo.Context) error {
	res := new([]result)
	err := c.Bind(res)
	if err != nil {
		return c.String(http.StatusInternalServerError, err.Error())
	}

	err = save(*res)
	if err != nil {
		return c.String(http.StatusInternalServerError, err.Error())
	}

	return c.NoContent(http.StatusOK)
}

func main() {
	httpAddr := flag.String("http.addr", ":80", "HTTP address:port")
	httpsAddr := flag.String("https.addr", ":443", "HTTPS address:port")
	tls := flag.Bool("tls", false, "Enable AutoTLS")
	tlsHostname := flag.String("tls.hostname", "", "(Optional) Hostname to restrict AutoTLS")
	flag.Parse()

	t := &Template{
		templates: template.Must(template.ParseGlob("views/*.html")),
	}

	e := echo.New()

	if *tls {
		if *tlsHostname != "" {
			e.AutoTLSManager.HostPolicy = autocert.HostWhitelist(*tlsHostname)
		}
		e.AutoTLSManager.Cache = autocert.DirCache(".cache")
		e.Pre(middleware.HTTPSRedirect())
	}

	e.Renderer = t
	e.Use(middleware.Logger())
	e.GET("/", index)
	e.GET("/ips.json", ips)
	e.POST("/results", recvResults)
	e.Static("/static", "static")

	if *tls {
		go func() { e.Logger.Fatal(e.Start(*httpAddr)) }()
		e.Logger.Fatal(e.StartAutoTLS(*httpsAddr))
	}
	e.Logger.Fatal(e.Start(*httpAddr))
}