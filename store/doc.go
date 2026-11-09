// Package store is the domain persistence layer for file-like artifacts: pages,
// layouts, components, and files of any kind. It owns their rows in of_files,
// content-addressed body snapshots for restore and diff, page-asset links, the
// symbol index used by resolve/where_used, and helpers for content gates, PDF
// rendering, and headless-browser probing. It sits above package db (raw SQLite)
// and is the single write path tools and the engine use for artifacts.
package store
