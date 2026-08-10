package main

import (
	"context"
	"fmt"
	"github.com/DataDog/package-blast-radius/internal/blast"
)

func main() {
	names := []string{"express", "react", "lodash"}
	aff := make([]blast.AffectedPackage, len(names))
	for i, n := range names {
		aff[i] = blast.AffectedPackage{
			PackageVersion:  blast.PackageVersion{System: blast.NPM, Name: n},
			WeeklyDownloads: -1,
		}
	}
	err := blast.Enrich(context.Background(), blast.NPM, aff, 2)
	fmt.Println("err:", err)
	for i, n := range names {
		fmt.Printf("  %s -> %d\n", n, aff[i].WeeklyDownloads)
	}
}
