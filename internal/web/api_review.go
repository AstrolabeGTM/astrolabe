package web

// Weekly review, updates list and content routes: each calls its MCP tool.
func (srv *Server) reviewRoutes() []apiRoute {
	return []apiRoute{
		srv.toolRoute("GET /api/review", "weekly_review"),
		srv.toolRoute("POST /api/review/save", "save_review"),
		srv.toolRoute("GET /api/review/saved", "reviews"),
	}
}
