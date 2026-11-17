// Package memory hosts the dashboard's reads of the Claude CLI's auto-memory
// (per-project state dirs): /api/memory/{slug} for [[slug]] hover cards, and
// /api/memories[/file] for the settings view's 懂你 section, which also edits
// and deletes them from this machine only.
package memory
