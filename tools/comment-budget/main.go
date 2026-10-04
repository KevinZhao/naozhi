// Command comment-budget reports how many comments break CLAUDE.md's Code
// Comments rules, and where (#2897 G7). The counts are ratcheted by
// TestBudget; this prints them with the offending lines:
//
//	go run ./tools/comment-budget [-list InFuncBlocksOver5]
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	list := flag.String("list", "", "print the file:line hits of one counter")
	flag.Parse()
	c, err := count(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "comment-budget:", err)
		os.Exit(2)
	}
	fmt.Printf("InFuncBlocksOver5 %d\nDocOver10 %d\nPackageDocOver60 %d\nReviewAnchors %d\nHistoryPhrases %d\nDuplicateComments %d\nIssueRefs %d\n",
		c.InFuncBlocksOver5, c.DocOver10, c.PackageDocOver60, c.ReviewAnchors, c.HistoryPhrases, c.DuplicateComments, c.IssueRefs)
	for _, h := range c.Offenders[*list] {
		fmt.Println(h)
	}
}
