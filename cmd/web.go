// Copyright 2014 The Gogs Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package cmd

import (
	"crypto/tls"
	"fmt"
	gotmpl "html/template"
	"io/ioutil"
	"net/http"
	"net/http/fcgi"
	"os"
	"path"
	"strings"

	"github.com/codegangsta/cli"
	"github.com/go-macaron/binding"
	"github.com/go-macaron/cache"
	"github.com/go-macaron/captcha"
	"github.com/go-macaron/csrf"
	"github.com/go-macaron/gzip"
	"github.com/go-macaron/i18n"
	"github.com/go-macaron/session"
	"github.com/go-macaron/toolbox"
	"github.com/go-xorm/xorm"
	"github.com/mcuadros/go-version"
	"gopkg.in/ini.v1"
	"gopkg.in/macaron.v1"

	api "github.com/gogits/go-gogs-client"

	"github.com/gogits/gogs/models"
	"github.com/gogits/gogs/modules/auth"
	"github.com/gogits/gogs/modules/auth/apiv1"
	"github.com/gogits/gogs/modules/avatar"
	"github.com/gogits/gogs/modules/bindata"
	"github.com/gogits/gogs/modules/log"
	"github.com/gogits/gogs/modules/middleware"
	"github.com/gogits/gogs/modules/setting"
	"github.com/gogits/gogs/modules/template"
	"github.com/gogits/gogs/routers"
	"github.com/gogits/gogs/routers/admin"
	"github.com/gogits/gogs/routers/api/v1"
	"github.com/gogits/gogs/routers/dev"
	"github.com/gogits/gogs/routers/org"
	"github.com/gogits/gogs/routers/repo"
	"github.com/gogits/gogs/routers/user"
)

var CmdWeb = cli.Command{
	Name:  "web",
	Usage: "Start Gogs web server",
	Description: `Gogs web server is the only thing you need to run,
and it takes care of all the other things for you`,
	Action: runWeb,
	Flags: []cli.Flag{
		stringFlag("port, p", "3000", "Temporary port number to prevent conflict"),
		stringFlag("config, c", "custom/conf/app.ini", "Custom configuration file path"),
	},
}

type VerChecker struct {
	ImportPath string
	Version    func() string
	Expected   string
}

// checkVersion checks if binary matches the version of templates files.
func checkVersion() {
	// Templates.
	data, err := ioutil.ReadFile(setting.StaticRootPath + "/templates/.VERSION")
	if err != nil {
		log.Fatal(4, "Fail to read 'templates/.VERSION': %v", err)
	}
	if string(data) != setting.AppVer {
		log.Fatal(4, "Binary and template file version does not match, did you forget to recompile?")
	}

	// Check dependency version.
	checkers := []VerChecker{
		{"github.com/go-xorm/xorm", func() string { return xorm.Version }, "0.4.4.1029"},
		{"github.com/Unknwon/macaron", macaron.Version, "0.5.4"},
		{"github.com/go-macaron/binding", binding.Version, "0.1.0"},
		{"github.com/go-macaron/cache", cache.Version, "0.1.2"},
		{"github.com/go-macaron/csrf", csrf.Version, "0.0.3"},
		{"github.com/go-macaron/i18n", i18n.Version, "0.0.7"},
		{"github.com/go-macaron/session", session.Version, "0.1.6"},
		{"github.com/go-macaron/toolbox", toolbox.Version, "0.1.0"},
		{"gopkg.in/ini.v1", ini.Version, "1.3.4"},
	}
	for _, c := range checkers {
		if !version.Compare(c.Version(), c.Expected, ">=") {
			log.Fatal(4, "Package '%s' version is too old(%s -> %s), did you forget to update?", c.ImportPath, c.Version(), c.Expected)
		}
	}
}

// newMacaron initializes Macaron instance.
func newMacaron() *macaron.Macaron {
	m := macaron.New()
	if !setting.DisableRouterLog {
		m.Use(macaron.Logger())
	}
	m.Use(macaron.Recovery())
	if setting.EnableGzip {
		m.Use(gzip.Gziper())
	}
	if setting.Protocol == setting.FCGI {
		m.SetURLPrefix(setting.AppSubUrl)
	}
	m.Use(macaron.Static(
		path.Join(setting.StaticRootPath, "public"),
		macaron.StaticOptions{
			SkipLogging: setting.DisableRouterLog,
		},
	))
	m.Use(macaron.Static(
		setting.AvatarUploadPath,
		macaron.StaticOptions{
			Prefix:      "avatars",
			SkipLogging: setting.DisableRouterLog,
		},
	))
	m.Use(macaron.Renderer(macaron.RenderOptions{
		Directory:  path.Join(setting.StaticRootPath, "templates"),
		Funcs:      []gotmpl.FuncMap{template.Funcs},
		IndentJSON: macaron.Env != macaron.PROD,
	}))

	localeNames, err := bindata.AssetDir("conf/locale")
	if err != nil {
		log.Fatal(4, "Fail to list locale files: %v", err)
	}
	localFiles := make(map[string][]byte)
	for _, name := range localeNames {
		localFiles[name] = bindata.MustAsset("conf/locale/" + name)
	}
	m.Use(i18n.I18n(i18n.Options{
		SubURL:          setting.AppSubUrl,
		Files:           localFiles,
		CustomDirectory: path.Join(setting.CustomPath, "conf/locale"),
		Langs:           setting.Langs,
		Names:           setting.Names,
		Redirect:        true,
	}))
	m.Use(cache.Cacher(cache.Options{
		Adapter:       setting.CacheAdapter,
		AdapterConfig: setting.CacheConn,
		Interval:      setting.CacheInternal,
	}))
	m.Use(captcha.Captchaer(captcha.Options{
		SubURL: setting.AppSubUrl,
	}))
	m.Use(session.Sessioner(setting.SessionConfig))
	m.Use(csrf.Csrfer(csrf.Options{
		Secret:     setting.SecretKey,
		SetCookie:  true,
		Header:     "X-Csrf-Token",
		CookiePath: setting.AppSubUrl,
	}))
	m.Use(toolbox.Toolboxer(m, toolbox.Options{
		HealthCheckFuncs: []*toolbox.HealthCheckFuncDesc{
			&toolbox.HealthCheckFuncDesc{
				Desc: "Database connection",
				Func: models.Ping,
			},
		},
	}))
	m.Use(middleware.Contexter())
	return m
}

func runWeb(ctx *cli.Context) {
	if ctx.IsSet("config") {
		setting.CustomConf = ctx.String("config")
	}
	routers.GlobalInit()
	checkVersion()

	m := newMacaron()

	reqSignIn := middleware.Toggle(&middleware.ToggleOptions{SignInRequire: true})
	ignSignIn := middleware.Toggle(&middleware.ToggleOptions{SignInRequire: setting.Service.RequireSignInView})
	ignSignInAndCsrf := middleware.Toggle(&middleware.ToggleOptions{DisableCsrf: true})
	reqSignOut := middleware.Toggle(&middleware.ToggleOptions{SignOutRequire: true})

	bind := binding.Bind
	bindIgnErr := binding.BindIgnErr

	// Routers.
	m.Get("/", ignSignIn, routers.Home)
	m.Get("/explore", ignSignIn, routers.Explore)
	m.Combo("/install", routers.InstallInit).Get(routers.Install).
		Post(bindIgnErr(auth.InstallForm{}), routers.InstallPost)
	m.Get("/^:type(issues|pulls)$", reqSignIn, user.Issues)

	// ***** START: API *****
	// FIXME: custom form error response.
	m.Group("/api", func() {
		m.Group("/v1", func() {
			// Miscellaneous.
			m.Post("/markdown", bindIgnErr(apiv1.MarkdownForm{}), v1.Markdown)
			m.Post("/markdown/raw", v1.MarkdownRaw)

			// Users.
			m.Group("/users", func() {
				m.Get("/search", v1.SearchUsers)

				m.Group("/:username", func() {
					m.Get("", v1.GetUserInfo)

					m.Group("/tokens", func() {
						m.Combo("").Get(v1.ListAccessTokens).
							Post(bind(v1.CreateAccessTokenForm{}), v1.CreateAccessToken)
					}, middleware.ApiReqBasicAuth())
				})
			})

			// Repositories.
			m.Combo("/user/repos", middleware.ApiReqToken()).Get(v1.ListMyRepos).
				Post(bind(api.CreateRepoOption{}), v1.CreateRepo)
			m.Post("/org/:org/repos", middleware.ApiReqToken(), bind(api.CreateRepoOption{}), v1.CreateOrgRepo)

			m.Group("/repos", func() {
				m.Get("/search", v1.SearchRepos)
			})

			m.Group("/repos", func() {
				m.Post("/migrate", bindIgnErr(auth.MigrateRepoForm{}), v1.MigrateRepo)
				m.Combo("/:username/:reponame").Get(v1.GetRepo).
					Delete(v1.DeleteRepo)

				m.Group("/:username/:reponame", func() {
					m.Combo("/hooks").Get(v1.ListRepoHooks).
						Post(bind(api.CreateHookOption{}), v1.CreateRepoHook)
					m.Patch("/hooks/:id:int", bind(api.EditHookOption{}), v1.EditRepoHook)
					m.Get("/raw/*", middleware.RepoRef(), v1.GetRepoRawFile)
					m.Get("/archive/*", v1.GetRepoArchive)

					m.Group("/keys", func() {
						m.Combo("").Get(v1.ListRepoDeployKeys).
							Post(bind(api.CreateDeployKeyOption{}), v1.CreateRepoDeployKey)
						m.Combo("/:id").Get(v1.GetRepoDeployKey).
							Delete(v1.DeleteRepoDeploykey)
					})
				}, middleware.ApiRepoAssignment())
			}, middleware.ApiReqToken())

			m.Any("/*", func(ctx *middleware.Context) {
				ctx.Error(404)
			})
		})
	}, ignSignIn)
	// ***** END: API *****

	// ***** START: User *****
	m.Group("/user", func() {
		m.Get("/login", user.SignIn)
		m.Post("/login", bindIgnErr(auth.SignInForm{}), user.SignInPost)
		m.Get("/sign_up", user.SignUp)
		m.Post("/sign_up", bindIgnErr(auth.RegisterForm{}), user.SignUpPost)
		m.Get("/reset_password", user.ResetPasswd)
		m.Post("/reset_password", user.ResetPasswdPost)
	}, reqSignOut)

	m.Group("/user/settings", func() {
		m.Get("", user.Settings)
		m.Post("", bindIgnErr(auth.UpdateProfileForm{}), user.SettingsPost)
		m.Post("/avatar", binding.MultipartForm(auth.UploadAvatarForm{}), user.SettingsAvatar)
		m.Combo("/email").Get(user.SettingsEmails).
			Post(bindIgnErr(auth.AddEmailForm{}), user.SettingsEmailPost)
		m.Post("/email/delete", user.DeleteEmail)
		m.Get("/password", user.SettingsPassword)
		m.Post("/password", bindIgnErr(auth.ChangePasswordForm{}), user.SettingsPasswordPost)
		m.Combo("/ssh").Get(user.SettingsSSHKeys).
			Post(bindIgnErr(auth.AddSSHKeyForm{}), user.SettingsSSHKeysPost)
		m.Post("/ssh/delete", user.DeleteSSHKey)
		m.Combo("/applications").Get(user.SettingsApplications).
			Post(bindIgnErr(auth.NewAccessTokenForm{}), user.SettingsApplicationsPost)
		m.Post("/applications/delete", user.SettingsDeleteApplication)
		m.Route("/delete", "GET,POST", user.SettingsDelete)
	}, reqSignIn, func(ctx *middleware.Context) {
		ctx.Data["PageIsUserSettings"] = true
	})

	m.Group("/user", func() {
		// r.Get("/feeds", binding.Bind(auth.FeedsForm{}), user.Feeds)
		m.Any("/activate", user.Activate)
		m.Any("/activate_email", user.ActivateEmail)
		m.Get("/email2user", user.Email2User)
		m.Get("/forget_password", user.ForgotPasswd)
		m.Post("/forget_password", user.ForgotPasswdPost)
		m.Get("/logout", user.SignOut)
	})
	// ***** END: User *****

	// Gravatar service.
	avt := avatar.CacheServer("public/img/avatar/", "public/img/avatar_default.jpg")
	os.MkdirAll("public/img/avatar/", os.ModePerm)
	m.Get("/avatar/:hash", avt.ServeHTTP)

	adminReq := middleware.Toggle(&middleware.ToggleOptions{SignInRequire: true, AdminRequire: true})

	// ***** START: Admin *****
	m.Group("/admin", func() {
		m.Get("", adminReq, admin.Dashboard)
		m.Get("/config", admin.Config)
		m.Get("/monitor", admin.Monitor)

		m.Group("/users", func() {
			m.Get("", admin.Users)
			m.Get("/new", admin.NewUser)
			m.Post("/new", bindIgnErr(auth.AdminCrateUserForm{}), admin.NewUserPost)
			m.Get("/:userid", admin.EditUser)
			m.Post("/:userid", bindIgnErr(auth.AdminEditUserForm{}), admin.EditUserPost)
			m.Post("/:userid/delete", admin.DeleteUser)
		})

		m.Group("/orgs", func() {
			m.Get("", admin.Organizations)
		})

		m.Group("/repos", func() {
			m.Get("", admin.Repositories)
		})

		m.Group("/auths", func() {
			m.Get("", admin.Authentications)
			m.Get("/new", admin.NewAuthSource)
			m.Post("/new", bindIgnErr(auth.AuthenticationForm{}), admin.NewAuthSourcePost)
			m.Combo("/:authid").Get(admin.EditAuthSource).
				Post(bindIgnErr(auth.AuthenticationForm{}), admin.EditAuthSourcePost)
			m.Post("/:authid/delete", admin.DeleteAuthSource)
		})

		m.Group("/notices", func() {
			m.Get("", admin.Notices)
			m.Get("/:id:int/delete", admin.DeleteNotice)
		})
	}, adminReq)
	// ***** END: Admin *****

	m.Group("", func() {
		m.Get("/:username", user.Profile)
		m.Get("/attachments/:uuid", func(ctx *middleware.Context) {
			attach, err := models.GetAttachmentByUUID(ctx.Params(":uuid"))
			if err != nil {
				if models.IsErrAttachmentNotExist(err) {
					ctx.Error(404)
				} else {
					ctx.Handle(500, "GetAttachmentByUUID", err)
				}
				return
			}

			fr, err := os.Open(attach.LocalPath())
			if err != nil {
				ctx.Handle(500, "Open", err)
				return
			}
			defer fr.Close()

			ctx.Header().Set("Cache-Control", "public,max-age=86400")
			// Fix #312. Attachments with , in their name are not handled correctly by Google Chrome.
			// We must put the name in " manually.
			if err = repo.ServeData(ctx, "\""+attach.Name+"\"", fr); err != nil {
				ctx.Handle(500, "ServeData", err)
				return
			}
		})
		m.Post("/issues/attachments", repo.UploadIssueAttachment)
	}, ignSignIn)

	if macaron.Env == macaron.DEV {
		m.Get("/template/*", dev.TemplatePreview)
	}

	reqRepoAdmin := middleware.RequireRepoAdmin()
	reqRepoPusher := middleware.RequireRepoPusher()

	// ***** START: Organization *****
	m.Group("/org", func() {
		m.Get("/create", org.Create)
		m.Post("/create", bindIgnErr(auth.CreateOrgForm{}), org.CreatePost)

		m.Group("/:org", func() {
			m.Get("/dashboard", user.Dashboard)
			m.Get("/^:type(issues|pulls)$", user.Issues)
			m.Get("/members", org.Members)
			m.Get("/members/action/:action", org.MembersAction)

			m.Get("/teams", org.Teams)
			m.Get("/teams/:team", org.TeamMembers)
			m.Get("/teams/:team/repositories", org.TeamRepositories)
			m.Route("/teams/:team/action/:action", "GET,POST", org.TeamsAction)
			m.Route("/teams/:team/action/repo/:action", "GET,POST", org.TeamsRepoAction)
		}, middleware.OrgAssignment(true))

		m.Group("/:org", func() {
			m.Get("/teams/new", org.NewTeam)
			m.Post("/teams/new", bindIgnErr(auth.CreateTeamForm{}), org.NewTeamPost)
			m.Get("/teams/:team/edit", org.EditTeam)
			m.Post("/teams/:team/edit", bindIgnErr(auth.CreateTeamForm{}), org.EditTeamPost)
			m.Post("/teams/:team/delete", org.DeleteTeam)

			m.Group("/settings", func() {
				m.Combo("").Get(org.Settings).
					Post(bindIgnErr(auth.UpdateOrgSettingForm{}), org.SettingsPost)
				m.Post("/avatar", binding.MultipartForm(auth.UploadAvatarForm{}), org.SettingsAvatar)

				m.Group("/hooks", func() {
					m.Get("", org.Webhooks)
					m.Post("/delete", org.DeleteWebhook)
					m.Get("/:type/new", repo.WebhooksNew)
					m.Post("/gogs/new", bindIgnErr(auth.NewWebhookForm{}), repo.WebHooksNewPost)
					m.Post("/slack/new", bindIgnErr(auth.NewSlackHookForm{}), repo.SlackHooksNewPost)
					m.Get("/:id", repo.WebHooksEdit)
					m.Post("/gogs/:id", bindIgnErr(auth.NewWebhookForm{}), repo.WebHooksEditPost)
					m.Post("/slack/:id", bindIgnErr(auth.NewSlackHookForm{}), repo.SlackHooksEditPost)
				})

				m.Route("/delete", "GET,POST", org.SettingsDelete)
			})

			m.Route("/invitations/new", "GET,POST", org.Invitation)
		}, middleware.OrgAssignment(true, true))
	}, reqSignIn)
	// ***** END: Organization *****

	// ***** START: Repository *****
	m.Group("/repo", func() {
		m.Get("/create", repo.Create)
		m.Post("/create", bindIgnErr(auth.CreateRepoForm{}), repo.CreatePost)
		m.Get("/migrate", repo.Migrate)
		m.Post("/migrate", bindIgnErr(auth.MigrateRepoForm{}), repo.MigratePost)
		m.Combo("/fork/:repoid").Get(repo.Fork).
			Post(bindIgnErr(auth.CreateRepoForm{}), repo.ForkPost)
	}, reqSignIn)

	m.Group("/:username/:reponame", func() {
		m.Group("/settings", func() {
			m.Combo("").Get(repo.Settings).
				Post(bindIgnErr(auth.RepoSettingForm{}), repo.SettingsPost)
			m.Route("/collaboration", "GET,POST", repo.Collaboration)

			m.Group("/hooks", func() {
				m.Get("", repo.Webhooks)
				m.Post("/delete", repo.DeleteWebhook)
				m.Get("/:type/new", repo.WebhooksNew)
				m.Post("/gogs/new", bindIgnErr(auth.NewWebhookForm{}), repo.WebHooksNewPost)
				m.Post("/slack/new", bindIgnErr(auth.NewSlackHookForm{}), repo.SlackHooksNewPost)
				m.Get("/:id", repo.WebHooksEdit)
				m.Post("/gogs/:id", bindIgnErr(auth.NewWebhookForm{}), repo.WebHooksEditPost)
				m.Post("/slack/:id", bindIgnErr(auth.NewSlackHookForm{}), repo.SlackHooksEditPost)

				m.Group("/git", func() {
					m.Get("", repo.GitHooks)
					m.Combo("/:name").Get(repo.GitHooksEdit).
						Post(repo.GitHooksEditPost)
				}, middleware.GitHookService())
			})

			m.Group("/keys", func() {
				m.Combo("").Get(repo.DeployKeys).
					Post(bindIgnErr(auth.AddSSHKeyForm{}), repo.DeployKeysPost)
				m.Post("/delete", repo.DeleteDeployKey)
			})

		}, func(ctx *middleware.Context) {
			ctx.Data["PageIsSettings"] = true
		})
	}, reqSignIn, middleware.RepoAssignment(), reqRepoAdmin, middleware.RepoRef())

	m.Group("/:username/:reponame", func() {
		m.Get("/action/:action", repo.Action)

		m.Group("/issues", func() {
			m.Combo("/new").Get(repo.NewIssue).
				Post(bindIgnErr(auth.CreateIssueForm{}), repo.NewIssuePost)

			m.Combo("/:index/comments").Post(bindIgnErr(auth.CreateCommentForm{}), repo.NewComment)
			m.Group("/:index", func() {
				m.Post("/label", repo.UpdateIssueLabel)
				m.Post("/milestone", repo.UpdateIssueMilestone)
				m.Post("/assignee", repo.UpdateIssueAssignee)
			}, reqRepoAdmin)

			m.Group("/:index", func() {
				m.Post("/title", repo.UpdateIssueTitle)
				m.Post("/content", repo.UpdateIssueContent)
			})
		})
		m.Post("/comments/:id", repo.UpdateCommentContent)
		m.Group("/labels", func() {
			m.Post("/new", bindIgnErr(auth.CreateLabelForm{}), repo.NewLabel)
			m.Post("/edit", bindIgnErr(auth.CreateLabelForm{}), repo.UpdateLabel)
			m.Post("/delete", repo.DeleteLabel)
		}, reqRepoAdmin)
		m.Group("/milestones", func() {
			m.Get("/new", repo.NewMilestone)
			m.Post("/new", bindIgnErr(auth.CreateMilestoneForm{}), repo.NewMilestonePost)
			m.Get("/:id/edit", repo.EditMilestone)
			m.Post("/:id/edit", bindIgnErr(auth.CreateMilestoneForm{}), repo.EditMilestonePost)
			m.Get("/:id/:action", repo.ChangeMilestonStatus)
			m.Post("/delete", repo.DeleteMilestone)
		}, reqRepoAdmin)

		m.Group("/releases", func() {
			m.Get("/new", repo.NewRelease)
			m.Post("/new", bindIgnErr(auth.NewReleaseForm{}), repo.NewReleasePost)
			m.Get("/edit/:tagname", repo.EditRelease)
			m.Post("/edit/:tagname", bindIgnErr(auth.EditReleaseForm{}), repo.EditReleasePost)
			m.Post("/delete", repo.DeleteRelease)
		}, reqRepoAdmin, middleware.RepoRef())

		m.Combo("/compare/*").Get(repo.CompareAndPullRequest).
			Post(bindIgnErr(auth.CreateIssueForm{}), repo.CompareAndPullRequestPost)
	}, reqSignIn, middleware.RepoAssignment())

	m.Group("/:username/:reponame", func() {
		m.Group("", func() {
			m.Get("/releases", repo.Releases)
			m.Get("/^:type(issues|pulls)$", repo.RetrieveLabels, repo.Issues)
			m.Get("/labels/", repo.RetrieveLabels, repo.Labels)
			m.Get("/milestones", repo.Milestones)
		}, middleware.RepoRef(),
			func(ctx *middleware.Context) {
				ctx.Data["PageIsList"] = true
			})
		m.Get("/^:type(issues|pulls)$/:index", repo.ViewIssue)

		// m.Get("/branches", repo.Branches)

		m.Group("/wiki", func() {
			m.Get("/?:page", repo.Wiki)
			m.Get("/_pages", repo.WikiPages)

			m.Group("", func() {
				m.Combo("/_new").Get(repo.NewWiki).
					Post(bindIgnErr(auth.NewWikiForm{}), repo.NewWikiPost)
				m.Combo("/:page/_edit").Get(repo.EditWiki).
					Post(bindIgnErr(auth.NewWikiForm{}), repo.EditWikiPost)
			}, reqSignIn, reqRepoPusher)
		}, middleware.RepoRef())

		m.Get("/archive/*", repo.Download)

		m.Group("/pulls/:index", func() {
			m.Get("/commits", repo.ViewPullCommits)
			m.Get("/files", repo.ViewPullFiles)
			m.Post("/merge", reqRepoAdmin, repo.MergePullRequest)
		})

		m.Group("", func() {
			m.Get("/src/*", repo.Home)
			m.Get("/raw/*", repo.SingleDownload)
			m.Get("/commits/*", repo.RefCommits)
			m.Get("/commit/*", repo.Diff)
			m.Get("/stars", repo.Stars)
			m.Get("/watchers", repo.Watchers)
			m.Get("/forks", repo.Forks)
		}, middleware.RepoRef())

		m.Get("/compare/:before([a-z0-9]{40})...:after([a-z0-9]{40})", repo.CompareDiff)
	}, ignSignIn, middleware.RepoAssignment())

	m.Group("/:username", func() {
		m.Group("/:reponame", func() {
			m.Get("", repo.Home)
			m.Get("\\.git$", repo.Home)
		}, ignSignIn, middleware.RepoAssignment(true), middleware.RepoRef())

		m.Group("/:reponame", func() {
			m.Any("/*", ignSignInAndCsrf, repo.HTTP)
			m.Head("/tasks/trigger", repo.TriggerTask)
		})
	})
	// ***** END: Repository *****

	// robots.txt
	m.Get("/robots.txt", func(ctx *middleware.Context) {
		if setting.HasRobotsTxt {
			ctx.ServeFileContent(path.Join(setting.CustomPath, "robots.txt"))
		} else {
			ctx.Error(404)
		}
	})

	// Not found handler.
	m.NotFound(routers.NotFound)

	// Flag for port number in case first time run conflict.
	if ctx.IsSet("port") {
		setting.AppUrl = strings.Replace(setting.AppUrl, setting.HttpPort, ctx.String("port"), 1)
		setting.HttpPort = ctx.String("port")
	}

	var err error
	listenAddr := fmt.Sprintf("%s:%s", setting.HttpAddr, setting.HttpPort)
	log.Info("Listen: %v://%s%s", setting.Protocol, listenAddr, setting.AppSubUrl)
	switch setting.Protocol {
	case setting.HTTP:
		err = http.ListenAndServe(listenAddr, m)
	case setting.HTTPS:
		server := &http.Server{Addr: listenAddr, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS10}, Handler: m}
		err = server.ListenAndServeTLS(setting.CertFile, setting.KeyFile)
	case setting.FCGI:
		err = fcgi.Serve(nil, m)
	default:
		log.Fatal(4, "Invalid protocol: %s", setting.Protocol)
	}

	if err != nil {
		log.Fatal(4, "Fail to start server: %v", err)
	}
}
