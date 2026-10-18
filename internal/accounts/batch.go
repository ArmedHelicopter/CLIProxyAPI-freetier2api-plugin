package accounts

// 本文件提供账号批处理的通用并发执行器。
// admin 的 quota/keepalive 与 tasks 队列共用，避免各自复制 semaphore/done 模式。

import "sync"

// RunBatch 以固定并发度执行 fn，并保持输入顺序返回结果。
func RunBatch[T any, R any](items []T, concurrency int, fn func(T) R) []R {
	results := make([]R, len(items))
	if len(items) == 0 {
		return results
	}
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > len(items) {
		concurrency = len(items)
	}
	semaphore := make(chan struct{}, concurrency)
	var waitGroup sync.WaitGroup
	for index := range items {
		waitGroup.Add(1)
		go func(slot int) {
			defer waitGroup.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			results[slot] = fn(items[slot])
		}(index)
	}
	waitGroup.Wait()
	return results
}
