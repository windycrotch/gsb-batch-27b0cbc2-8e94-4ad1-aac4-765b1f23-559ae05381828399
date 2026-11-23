module github.com/sqweek/sqribe

go 1.27

require (
	github.com/golang/freetype v0.0.0-20170609003504-e2365dfdc4a0
	github.com/gordonklaus/portaudio v0.0.0-20261121085658-afc58c9e4446
	github.com/mattn/go-sqlite3 v1.14.53
	github.com/skelterjohn/go.wde v0.0.0-20190318181201-adc3f78cdb45
	github.com/sqweek/dialog v0.0.0-20260123140253-64c163d53aac
	github.com/sqweek/ffau v0.0.0-20170417145421-06541ede39b1
	github.com/sqweek/fluidsynth v0.0.0-20151217121601-462ff346de79
	github.com/sqweek/fs v0.0.0-20170225065354-8f2cd690098c
	github.com/sqweek/go-image.svg v0.0.0-20240504231520-0260c8b00bc0
	golang.org/x/image v0.48.0
)

require (
	github.com/AllenDang/w32 v0.0.0-20180428130237-ad0a36d80adc // indirect
	github.com/BurntSushi/freetype-go v0.0.0-20160129220410-b763ddbfe298 // indirect
	github.com/BurntSushi/graphics-go v0.0.0-20160129215708-b43f31a4a966 // indirect
	github.com/BurntSushi/xgb v0.0.0-20210121224620-deaf085860bc // indirect
	github.com/BurntSushi/xgbutil v0.0.0-20190907113008-ad855c713046 // indirect
	github.com/TheTitanrain/w32 v0.0.0-20180517000239-4f5cfb03fabf // indirect
)

replace (
	github.com/AllenDang/w32 => ./third_party/w32
	github.com/sqweek/ffau => ./third_party/ffau
	github.com/sqweek/fluidsynth => ./third_party/fluidsynth
)
