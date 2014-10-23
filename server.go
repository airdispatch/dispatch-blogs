package main

import (
	"airdispat.ch/errors"
	"airdispat.ch/identity"
	"airdispat.ch/message"
	"airdispat.ch/routing"
	"airdispat.ch/server"
	"airdispat.ch/wire"
	"fmt"
	"getmelange.com/router"
	"github.com/airdispatch/go-pressure"
	"github.com/russross/blackfriday"
	"html/template"
	"sort"
	"sync"
	"time"
)

func main() {
	s := pressure.CreateServer(":8000", true)

	id, err := identity.CreateIdentity()
	if err != nil {
		panic(err)
	}

	r := &router.Router{
		Origin: id,
		TrackerList: []string{
			"airdispatch.me",
		},
		Redirects: 10,
	}

	t := s.CreateTemplateEngine("templates", "base.html")

	p := CreatePosts()

	s.RegisterURL(
		// Homepage Route
		pressure.NewURLRoute("^/$", &HomepageController{
			Templates: t,
		}),

		pressure.NewURLRoute("^/goto$", &GotoController{}),

		// Viewer Routes
		pressure.NewURLRoute("^/view/(?P<alias>[^@]+@.+?)/(?P<name>.+)$", &ViewPostController{
			Templates: t,
			Posts:     p,
		}),
		pressure.NewURLRoute("^/view/(?P<alias>[^@]+@.+)$", &ViewerController{
			Posts:     p,
			Templates: t,
			Router:    r,
		}),
	)

	s.RunServer()
}

func errToHTTP(e error) *pressure.HTTPError {
	return &pressure.HTTPError{
		Code: 500,
		Text: e.Error(),
	}
}

type Collection []*Post

func (c Collection) Len() int           { return len(c) }
func (c Collection) Less(i, j int) bool { return c[i].Published.After(c[j].Published) }
func (c Collection) Swap(i, j int)      { c[i], c[j] = c[j], c[i] }

type Post struct {
	Title     string
	Body      template.HTML
	Author    string
	Name      string
	Published time.Time
}

type Posts struct {
	Data map[string]map[string]*Post
	Lock *sync.RWMutex
}

func CreatePosts() *Posts {
	return &Posts{
		Data: make(map[string]map[string]*Post),
		Lock: &sync.RWMutex{},
	}
}

func (p *Posts) GetPost(user, id string) *Post {
	p.Lock.RLock()
	defer p.Lock.RUnlock()

	u, ok := p.Data[user]
	if !ok {
		return nil
	}

	post, ok := u[id]
	if !ok {
		return nil
	}

	return post
}

func (p *Posts) StorePost(user, id string, post *Post) {
	p.Lock.Lock()
	defer p.Lock.Unlock()

	u, ok := p.Data[user]
	if !ok {
		u = make(map[string]*Post)
	}

	u[id] = post

	p.Data[user] = u
}

type GotoController struct{}

func (c *GotoController) GetResponse(r *pressure.Request, l *pressure.Logger) (pressure.View, *pressure.HTTPError) {
	return &RedirectView{
		Temporary: true,
		Location:  fmt.Sprintf("/view/%s", r.Form["address"][0]),
	}, nil
}

type RedirectView struct {
	pressure.BasicView
	Temporary bool
	Location  string
}

func (r *RedirectView) Headers() pressure.ViewHeaders {
	hdrs := r.BasicView.Headers()
	if hdrs == nil {
		hdrs = make(pressure.ViewHeaders)
	}

	hdrs["Location"] = r.Location
	return hdrs
}

func (r *RedirectView) StatusCode() int {
	if r.Temporary {
		return 302
	}
	return 301
}

type HomepageController struct {
	Templates *pressure.TemplateEngine
}

func (c *HomepageController) GetResponse(r *pressure.Request, l *pressure.Logger) (pressure.View, *pressure.HTTPError) {
	return c.Templates.NewTemplateView("home.html", map[string]interface{}{}), nil
}

type ViewPostController struct {
	Templates *pressure.TemplateEngine
	Posts     *Posts
}

func (v *ViewPostController) GetResponse(r *pressure.Request, l *pressure.Logger) (pressure.View, *pressure.HTTPError) {
	alias := r.URL["alias"]
	id := r.URL["name"]

	post := v.Posts.GetPost(alias, id)
	if post == nil {
		return nil, &pressure.HTTPError{
			Code: 404,
			Text: "Could not find that post.",
		}
	}

	return v.Templates.NewTemplateView("viewPost.html", map[string]interface{}{
		"user": alias,
		"post": post,
	}), nil
}

type ViewerController struct {
	Templates *pressure.TemplateEngine
	Router    *router.Router
	Posts     *Posts
}

func (v *ViewerController) GetResponse(r *pressure.Request, l *pressure.Logger) (pressure.View, *pressure.HTTPError) {
	alias := r.URL["alias"]
	srv, err := v.Router.LookupAlias(alias, routing.LookupTypeTX)
	if err != nil {
		return nil, errToHTTP(err)
	}

	author, err := v.Router.LookupAlias(alias, routing.LookupTypeMAIL)
	if err != nil {
		return nil, errToHTTP(err)
	}

	conn, err := message.ConnectToServer(srv.Location)
	if err != nil {
		return nil, errToHTTP(err)
	}

	txMsg := server.CreateTransferMessageList(0, v.Router.Origin.Address, srv, author)

	err = message.SignAndSendToConnection(txMsg, v.Router.Origin, srv, conn)
	if err != nil {
		return nil, errToHTTP(err)
	}

	recvd, err := message.ReadMessageFromConnection(conn)
	if err != nil {
		return nil, errToHTTP(err)
	}

	byt, typ, h, err := recvd.Reconstruct(v.Router.Origin, true)
	if err != nil {
		return nil, errToHTTP(err)
	}

	if typ == wire.ErrorCode {
		return nil, errToHTTP(
			errors.CreateErrorFromBytes(byt, h),
		)
	} else if typ != wire.MessageListCode {
		return nil, errToHTTP(
			fmt.Errorf("Expected type (%s), got type (%s)", wire.MessageListCode, typ),
		)
	}

	msgList, err := server.CreateMessageListFromBytes(byt, h)
	if err != nil {
		return nil, errToHTTP(err)
	}

	// Get all important messages
	var posts Collection

	for i := uint64(0); i < msgList.Length; i++ {
		newMsg, err := message.ReadMessageFromConnection(conn)
		if err != nil {
			return nil, errToHTTP(err)
		}

		byt, typ, h, err := newMsg.Reconstruct(v.Router.Origin, false)
		if err != nil {
			return nil, errToHTTP(err)
		}

		if typ == wire.ErrorCode {
			return nil, errToHTTP(
				errors.CreateErrorFromBytes(byt, h),
			)
		} else if typ != wire.MailCode {
			return nil, errToHTTP(
				fmt.Errorf("Expected type (%s), got type (%s)", wire.MailCode, typ),
			)
		}

		// Message is of the correct type.
		mail, err := message.CreateMailFromBytes(byt, h)
		if err != nil {
			return nil, errToHTTP(err)
		}

		if mail.Components.HasComponent("airdispat.ch/notes/title") {
			p := &Post{
				Title:     mail.Components.GetStringComponent("airdispat.ch/notes/title"),
				Body:      template.HTML(blackfriday.MarkdownCommon(mail.Components.GetComponent("airdispat.ch/notes/body"))),
				Author:    alias,
				Name:      mail.Name,
				Published: time.Unix(int64(h.Timestamp), 0),
			}

			posts = append(posts, p)
			v.Posts.StorePost(alias, p.Name, p)
		}
	}

	sort.Sort(posts)

	return v.Templates.NewTemplateView("viewer.html", map[string]interface{}{
		"user":  alias,
		"blogs": posts,
	}), nil
}
