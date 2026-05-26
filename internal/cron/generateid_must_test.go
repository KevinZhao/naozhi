package cron

// must_genid_test.go 是测试专用的便利包装，把 generateHexID /
// generateID / generateRunID 的 (string, error) 返回还原为单值；
// crypto/rand 失败在 test 进程里不可能发生（Linux 测试机 getrandom
// 一定可用），出错 panic 等价 t.Fatal —— 但这种 panic 仅限 _test.go
// 调用路径，绝不允许在生产代码中复用。R242-CR-14 (#706).
//
// 命名上加 must 前缀对齐 stdlib regexp.MustCompile / template.Must 的
// 「panic-on-error 便利」约定，与生产 helper 的「显式 error」语义形成
// 对比；reviewers 一眼就能看出哪段是 test-only 简化、哪段是必须把
// error 处理掉的真路径。

func mustGenerateHexID() string {
	id, err := generateHexID()
	if err != nil {
		panic("cron test: generateHexID: " + err.Error())
	}
	return id
}

func mustGenerateID() string {
	id, err := generateID()
	if err != nil {
		panic("cron test: generateID: " + err.Error())
	}
	return id
}

func mustGenerateRunID() string {
	id, err := generateRunID()
	if err != nil {
		panic("cron test: generateRunID: " + err.Error())
	}
	return id
}
