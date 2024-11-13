# gocovmerge

gocovmerge takes the results from multiple `go test -coverprofile` runs and
merges them into one profile.

## usage

```
gocovmerge [cover.txt.timestamp.hash...]
```

eg:
```
gocovmerge cover.txt.1723042827.e24dac6 cover.txt.1723042828.e24dac6
```


gocovmerge takes the source coverprofiles as the arguments (output from
`go test -coverprofile coverage.out`) and outputs a merged version of the
files to standard out. You can only merge profiles that were generated from the
same source code. If there are source lines that overlap or do not merge, the
process will exit with an error code.
