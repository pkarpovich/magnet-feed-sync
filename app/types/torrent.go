package types

type TorrentState struct {
	Hash         string
	Name         string
	State        string
	ContentPath  string
	Progress     float64
	CompletionOn int64
	Size         int64
}
