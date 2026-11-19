package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

var forkOwnedFiles = []string{
	"acp.go",
	"acp_conv.go",
	"acp_exec.go",
	"acp_media.go",
	"discovery_test.go",
	"comment_cap_test.go",
}

const maxCommentLines = 2

func TestForkOwnedCommentsStayWithinTwoLines(t *testing.T) {
	fset := token.NewFileSet()
	for _, name := range forkOwnedFiles {
		file, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, group := range file.Comments {
			if isDirective(group) {
				continue
			}
			start, end := fset.Position(group.Pos()), fset.Position(group.End())
			if lines := end.Line - start.Line + 1; lines > maxCommentLines {
				t.Errorf("%s:%d has a %d-line comment block; shorten it to %d lines, or move the fact into a better name or a doc",
					name, start.Line, lines, maxCommentLines)
			}
		}
	}
}

func isDirective(group *ast.CommentGroup) bool {
	return len(group.List) > 0 && len(group.List[0].Text) > 5 && group.List[0].Text[:5] == "//go:"
}
