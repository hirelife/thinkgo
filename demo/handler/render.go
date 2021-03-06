package handler

import (
	"github.com/henrylee2cn/thinkgo"
)

type Render struct {
	Title     string   `param:"in(query),nonzero"`
	Paragraph []string `param:"in(query),name(p),len(1:10)" regexp:"(^[\\w]*$)"`
}

func (r *Render) Serve(ctx *thinkgo.Context) error {
	return ctx.Render(200, thinkgo.JionStatic("render.html"), thinkgo.Map{
		"title": r.Title,
		"p":     r.Paragraph,
	})
}

func init() {
	thinkgo.Global.TemplateVariable("__PUBLIC__", "/syso")
}

func Index() thinkgo.HandlerFunc {
	return func(ctx *thinkgo.Context) error {
		return ctx.Render(200, "../_syso/index.html", thinkgo.Map{
			"TITLE":   "thinkgo",
			"VERSION": thinkgo.VERSION,
			"CONTENT": "Welcome To Thinkgo",
			"AUTHOR":  "HenryLee",
		})
	}
}
