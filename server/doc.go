// Package server is the HTTP layer. It composes two routers: an admin control
// plane (the embedded admin UI plus its APIs, subpackage admin) and the public
// runtime that serves generated projects (subpackage public), wired together
// with shared middleware and the dependency bundle in subpackage deps.
package server
