package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/olivere/elastic/v7"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

const indexPrefix = "kb-rev"

type Renderer struct {
	templates *template.Template
}

func (r *Renderer) Render(w io.Writer, name string, data interface{}, c echo.Context) error {
	if r.templates == nil {
		return errors.New("renderer not initialized")
	}
	return r.templates.ExecuteTemplate(w, name, data)
}

func exit(err *error) {
	if *err != nil {
		log.Println("exited with error:", (*err).Error())
		os.Exit(1)
	} else {
		log.Println("exited")
	}
}

func main() {
	var err error
	defer exit(&err)

	var (
		envElasticsearchURL      = strings.TrimSpace(os.Getenv("KB_ELASTICSEARCH_URL"))
		envElasticsearchUsername = strings.TrimSpace(os.Getenv("KB_ELASTICSEARCH_USERNAME"))
		envElasticsearchPassword = strings.TrimSpace(os.Getenv("KB_ELASTICSEARCH_PASSWORD"))
		envAccessToken           = strings.TrimSpace(os.Getenv("KB_ACCESS_TOKEN"))
		envBind                  = strings.TrimSpace(os.Getenv("KB_BIND"))
		envDebug, _              = strconv.ParseBool(strings.TrimSpace(os.Getenv("KB_DEBUG")))
	)

	_ = envElasticsearchURL

	var client *elastic.Client

	{
		opts := []elastic.ClientOptionFunc{
			elastic.SetURL(envElasticsearchURL),
			elastic.SetSniff(false),
		}
		if envElasticsearchUsername != "" && envElasticsearchPassword != "" {
			opts = append(opts, elastic.SetBasicAuth(envElasticsearchUsername, envElasticsearchPassword))
		}

		if client, err = elastic.Dial(opts...); err != nil {
			return
		}
	}

	renderer := &Renderer{}

	_ = client

	e := echo.New()
	e.Debug = envDebug
	e.HideBanner = true
	e.HidePort = true
	e.Renderer = renderer
	e.Use(middleware.Recover())
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if c.Path() != "/" && c.QueryParam("access_token") != envAccessToken {
				return c.String(http.StatusForbidden, "invalid access_token")
			} else {
				return next(c)
			}
		}
	})
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if renderer.templates == nil || envDebug {
				renderer.templates = template.Must(template.ParseGlob("views/*.gohtml"))
			}
			return next(c)
		}
	})
	e.GET("/", func(c echo.Context) (err error) {
		type DataIndex struct {
			Index string
			Rev   int
		}
		type DataKind struct {
			Kind  string
			Count int64
		}
		type Data struct {
			Kinds   []DataKind
			Indices []DataIndex
		}
		var data Data
		{
			var res elastic.CatIndicesResponse
			if res, err = client.CatIndices().Do(c.Request().Context()); err != nil {
				return
			}
			for _, item := range res {
				if !strings.HasPrefix(item.Index, indexPrefix) {
					continue
				}
				if rev, err := strconv.Atoi(strings.TrimPrefix(item.Index, indexPrefix)); err == nil {
					data.Indices = append(data.Indices, DataIndex{
						Index: item.Index,
						Rev:   rev,
					})
				}
			}
			sort.Slice(data.Indices, func(i, j int) bool {
				return data.Indices[i].Rev > data.Indices[j].Rev
			})
			if len(data.Indices) == 0 {
				data.Indices = append(data.Indices, DataIndex{
					Index: indexPrefix + "1",
					Rev:   1,
				})
			}
		}
		{
			var res *elastic.SearchResult
			if res, err = client.Search("kb-*").Size(0).Aggregation(
				"kinds", elastic.NewTermsAggregation().Field("kind").Size(9999),
			).Do(c.Request().Context()); err != nil {
				return
			}

			if items, _ := res.Aggregations.Terms("kinds"); items != nil {
				for _, bucket := range items.Buckets {
					data.Kinds = append(data.Kinds, DataKind{
						Kind:  fmt.Sprintf("%v", bucket.Key),
						Count: bucket.DocCount,
					})
				}
			}
		}
		return c.Render(http.StatusOK, "index", data)
	})

	chErr := make(chan error, 1)
	chSig := make(chan os.Signal, 1)
	signal.Notify(chSig, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		log.Println("listening at", envBind)
		chErr <- e.Start(envBind)
	}()

	select {
	case err = <-chErr:
		return
	case sig := <-chSig:
		log.Println("signal caught:", sig)
		time.Sleep(time.Second * 1)
		err = e.Shutdown(context.Background())
	}
}
