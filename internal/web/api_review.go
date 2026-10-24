package web

// Weekly review, updates list and content routes: each calls its MCP tool.
func (srv *Server) reviewRoutes() []apiRoute {
	return []apiRoute{
		srv.toolRoute("GET /api/review", "weekly_review"),
		srv.toolRoute("POST /api/review/save", "save_review"),
		srv.toolRoute("GET /api/review/saved", "reviews"),
	}
}

func (srv *Server) updatesRoutes() []apiRoute {
	return []apiRoute{
		srv.toolRoute("GET /api/updates/subscribers", "updates_subscribers"),
		srv.toolRoute("GET /api/updates/export", "updates_export"),
		srv.toolRoute("POST /api/updates/add", "updates_add"),
		srv.toolRoute("POST /api/updates/unsubscribe", "updates_unsubscribe"),
		srv.toolRoute("GET /api/broadcasts", "broadcast_status"),
		srv.toolRoute("POST /api/broadcasts/create", "broadcast_create"),
		srv.toolRoute("POST /api/broadcasts/edit", "broadcast_edit"),
		srv.toolRoute("POST /api/broadcasts/approve", "broadcast_approve"),
		srv.toolRoute("POST /api/broadcasts/cancel", "broadcast_cancel"),
	}
}

func (srv *Server) contentRoutes() []apiRoute {
	return []apiRoute{
		srv.toolRoute("GET /api/content/calendar", "content_calendar"),
		srv.toolRoute("POST /api/content/mark", "content_mark"),
		srv.toolRoute("POST /api/content", "content"),
	}
}
