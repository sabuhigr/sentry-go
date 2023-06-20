package sentry

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test ticker that ticks on demand instead of relying on go runtime timing.
type profilerTestTicker struct {
	t      *testing.T
	tick   chan time.Time
	ticked chan struct{}
}

func (t *profilerTestTicker) TickSource() <-chan time.Time {
	return t.tick
}

func (t *profilerTestTicker) Ticked() {
	t.ticked <- struct{}{}
}

func (t *profilerTestTicker) Stop() {}

// Sleeps before a tick to emulate a reasonable frequency of ticks, or they may all come at the same relative time.
// Then, sends a tick and waits for the profiler to process it.
func (t *profilerTestTicker) Tick() bool {
	time.Sleep(time.Millisecond)
	t.tick <- time.Now()
	select {
	case <-t.ticked:
		return true
	case <-time.After(1 * time.Second):
		t.t.Log("Timed out waiting for Ticked() to be called.")
		return false
	}
}

func setupProfilerTestTicker(t *testing.T) *profilerTestTicker {
	ticker := &profilerTestTicker{
		t:      t,
		tick:   make(chan time.Time, 1),
		ticked: make(chan struct{}),
	}
	profilerTickerFactory = func(d time.Duration) profilerTicker { return ticker }
	return ticker
}

func restoreProfilerTicker() {
	profilerTickerFactory = profilerTickerFactoryDefault
}

func TestProfilerCollection(t *testing.T) {
	t.Run("RealTicker", func(t *testing.T) {
		var require = require.New(t)
		var goID = getCurrentGoID()

		start := time.Now()
		profiler := startProfiling(start)
		defer profiler.Stop(false)
		if isCI() {
			doWorkFor(5 * time.Second)
		} else {
			doWorkFor(35 * time.Millisecond)
		}
		end := time.Now()
		result := profiler.GetSlice(start, end)
		require.NotNil(result)
		require.Greater(result.callerGoID, uint64(0))
		require.Equal(goID, result.callerGoID)
		validateProfile(t, result.trace, end.Sub(start))
	})

	t.Run("CustomTicker", func(t *testing.T) {
		var require = require.New(t)
		var goID = getCurrentGoID()

		ticker := setupProfilerTestTicker(t)
		defer restoreProfilerTicker()

		start := time.Now()
		profiler := startProfiling(start)
		defer profiler.Stop(false)
		require.True(ticker.Tick())
		end := time.Now()
		result := profiler.GetSlice(start, end)
		require.NotNil(result)
		require.Greater(result.callerGoID, uint64(0))
		require.Equal(goID, result.callerGoID)
		validateProfile(t, result.trace, end.Sub(start))

		// Another slice that has start time different than the profiler start time.
		start = end
		require.True(ticker.Tick())
		require.True(ticker.Tick())
		end = time.Now()
		result = profiler.GetSlice(start, end)
		validateProfile(t, result.trace, end.Sub(start))
	})
}

// Check the order of frames for a known stack trace (i.e. this test case).
func TestProfilerStackTrace(t *testing.T) {
	var require = require.New(t)

	ticker := setupProfilerTestTicker(t)
	defer restoreProfilerTicker()

	start := time.Now()
	profiler := startProfiling(start)
	defer profiler.Stop(false)
	require.True(ticker.Tick())
	result := profiler.GetSlice(start, time.Now())
	require.NotNil(result)

	var actual = ""
	for _, sample := range result.trace.Samples {
		if sample.ThreadID == result.callerGoID {
			t.Logf("Found a sample for the calling goroutine ID: %d", result.callerGoID)
			var stack = result.trace.Stacks[sample.StackID]
			for _, frameIndex := range stack {
				var frame = result.trace.Frames[frameIndex]
				actual += fmt.Sprintf("%s %s\n", frame.Module, frame.Function)
			}
			break
		}
	}
	require.NotZero(len(actual))
	actual = actual[:len(actual)-1] // remove trailing newline
	t.Log(actual)

	// Note: we can't check the exact stack trace because the profiler runs its own goroutine
	// And this test goroutine may be interrupted at multiple points.
	require.True(strings.HasSuffix(actual, `
github.com/getsentry/sentry-go TestProfilerStackTrace
testing tRunner
testing (*T).Run`))
}

func TestProfilerCollectsOnStart(t *testing.T) {
	var require = require.New(t)

	setupProfilerTestTicker(t)
	defer restoreProfilerTicker()

	start := time.Now()
	profiler := startProfiling(start)
	profiler.Stop(true)
	require.NotNil(profiler.(*profileRecorder).samplesBucketsHead.Value)
}

func TestProfilerPanicDuringStartup(t *testing.T) {
	var require = require.New(t)

	atomic.StoreInt64(&testProfilerPanic, -1)

	start := time.Now()
	profiler := startProfiling(start)
	defer profiler.Stop(false)
	// wait until the profiler has panicked
	for i := 0; i < 100 && atomic.LoadInt64(&testProfilerPanic) != 0; i++ {
		doWorkFor(10 * time.Millisecond)
	}
	result := profiler.GetSlice(start, time.Now())

	require.Zero(atomic.LoadInt64(&testProfilerPanic))
	require.Nil(result)
}

func TestProfilerPanicOnTick(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping in short mode because of the timeout we wait for in Tick() after the panic.")
	}

	var require = require.New(t)

	ticker := setupProfilerTestTicker(t)
	defer restoreProfilerTicker()

	// Panic after the first sample is collected.
	atomic.StoreInt64(&testProfilerPanic, 3)

	start := time.Now()
	profiler := startProfiling(start)
	defer profiler.Stop(false)
	require.True(ticker.Tick())
	require.False(ticker.Tick())

	end := time.Now()
	result := profiler.GetSlice(start, end)

	require.Zero(atomic.LoadInt64(&testProfilerPanic))
	require.NotNil(result)
	validateProfile(t, result.trace, end.Sub(start))
}

func TestProfilerPanicOnTickDirect(t *testing.T) {
	var require = require.New(t)

	profiler := newProfiler(time.Now())
	profiler.testProfilerPanic = 2

	// first tick won't panic
	profiler.onTick()
	samplesBucket := profiler.samplesBucketsHead.Value
	require.NotNil(samplesBucket)

	// This is normally handled by the profiler goroutine and stops the profiler.
	require.Panics(profiler.onTick)
	require.Equal(samplesBucket, profiler.samplesBucketsHead.Value)

	profiler.testProfilerPanic = 0

	profiler.onTick()
	require.NotEqual(samplesBucket, profiler.samplesBucketsHead.Value)
	require.NotNil(profiler.samplesBucketsHead.Value)
}

func doWorkFor(duration time.Duration) {
	start := time.Now()
	for time.Since(start) < duration {
		_ = findPrimeNumber(1000)
		runtime.Gosched()
	}
}

//nolint:unparam
func findPrimeNumber(n int) int {
	count := 0
	a := 2
	for count < n {
		b := 2
		prime := true // to check if found a prime
		for b*b <= a {
			if a%b == 0 {
				prime = false
				break
			}
			b++
		}
		if prime {
			count++
		}
		a++
	}
	return a - 1
}

func validateProfile(t *testing.T, trace *profileTrace, duration time.Duration) {
	var require = require.New(t)
	require.NotNil(trace)
	require.NotEmpty(trace.Samples)
	require.NotEmpty(trace.Stacks)
	require.NotEmpty(trace.Frames)
	require.NotEmpty(trace.ThreadMetadata)

	for _, sample := range trace.Samples {
		require.GreaterOrEqual(sample.ElapsedSinceStartNS, uint64(0))
		require.GreaterOrEqual(uint64(duration.Nanoseconds()), sample.ElapsedSinceStartNS)
		require.GreaterOrEqual(sample.StackID, 0)
		require.Less(sample.StackID, len(trace.Stacks))
		require.Contains(trace.ThreadMetadata, strconv.Itoa(int(sample.ThreadID)))
	}

	for _, thread := range trace.ThreadMetadata {
		require.NotEmpty(thread.Name)
	}

	for _, frame := range trace.Frames {
		require.NotEmpty(frame.Function)
		require.Greater(len(frame.AbsPath)+len(frame.Filename), 0)
		require.Greater(frame.Lineno, 0)
	}
}

func TestProfilerSamplingRate(t *testing.T) {
	if isCI() {
		t.Skip("Skipping on CI because the machines are too overloaded to provide consistent ticker resolution.")
	}
	if testing.Short() {
		t.Skip("Skipping in short mode.")
	}

	var require = require.New(t)

	start := time.Now()
	profiler := startProfiling(start)
	defer profiler.Stop(false)
	doWorkFor(500 * time.Millisecond)
	end := time.Now()
	result := profiler.GetSlice(start, end)

	require.NotEmpty(result.trace.Samples)
	var samplesByThread = map[uint64]uint64{}
	var outliersByThread = map[uint64]uint64{}
	var outliers = 0
	var lastLogTime = uint64(0)
	for _, sample := range result.trace.Samples {
		count := samplesByThread[sample.ThreadID]

		var lowerBound = count * uint64(profilerSamplingRate.Nanoseconds())
		var upperBound = (count + 1 + outliersByThread[sample.ThreadID]) * uint64(profilerSamplingRate.Nanoseconds())

		if lastLogTime != sample.ElapsedSinceStartNS {
			t.Logf("Sample %d (%d) should be between %d and %d", count, sample.ElapsedSinceStartNS, lowerBound, upperBound)
			lastLogTime = sample.ElapsedSinceStartNS
		}

		// We can check the lower bound explicitly, but the upper bound is problematic as some samples may get delayed.
		// Therefore, we collect the number of outliers and check if it's reasonably low.
		require.GreaterOrEqual(sample.ElapsedSinceStartNS, lowerBound)
		if sample.ElapsedSinceStartNS > upperBound {
			// We also increase the count by one to shift the followup samples too.
			outliersByThread[sample.ThreadID]++
			if int(outliersByThread[sample.ThreadID]) > outliers {
				outliers = int(outliersByThread[sample.ThreadID])
			}
		}

		samplesByThread[sample.ThreadID] = count + 1
	}

	require.Less(outliers, len(result.trace.Samples)/10)
}

func TestProfilerStackBufferGrowth(t *testing.T) {
	var require = require.New(t)
	profiler := newProfiler(time.Now())

	_ = profiler.collectRecords()

	profiler.stacksBuffer = make([]byte, 1)
	require.Equal(1, len(profiler.stacksBuffer))
	var bytesWithAutoAlloc = profiler.collectRecords()
	var lenAfterAutoAlloc = len(profiler.stacksBuffer)
	require.Greater(lenAfterAutoAlloc, 1)
	require.Greater(lenAfterAutoAlloc, len(bytesWithAutoAlloc))

	_ = profiler.collectRecords()
	require.Equal(lenAfterAutoAlloc, len(profiler.stacksBuffer))
}

func countSamples(profiler *profileRecorder) (value int) {
	profiler.samplesBucketsHead.Do(func(bucket interface{}) {
		if bucket != nil {
			value += len(bucket.(profileSamplesBucket))
		}
	})
	return value
}

// This tests profiler internals and replaces in-code asserts. While this shouldn't generally be done and instead
// we should test the profiler API only, this is trying to reduce a chance of a broken code that may externally work
// but has unbounded memory usage or similar performance issue.
func TestProfilerInternalMaps(t *testing.T) {
	var assert = assert.New(t)

	profiler := newProfiler(time.Now())

	// The size of the ring buffer is fixed throughout
	ringBufferSize := 3030

	// First, there is no data.
	assert.Zero(len(profiler.frames))
	assert.Zero(len(profiler.frameIndexes))
	assert.Zero(len(profiler.newFrames))
	assert.Zero(len(profiler.stacks))
	assert.Zero(len(profiler.stackIndexes))
	assert.Zero(len(profiler.newStacks))
	assert.Zero(len(profiler.routines))
	assert.Zero(countSamples(profiler))
	assert.Equal(ringBufferSize, profiler.samplesBucketsHead.Len())

	// After a tick, there is some data.
	profiler.onTick()
	assert.NotZero(len(profiler.frames))
	assert.NotZero(len(profiler.frameIndexes))
	assert.NotZero(len(profiler.newFrames))
	assert.NotZero(len(profiler.stacks))
	assert.NotZero(len(profiler.stackIndexes))
	assert.NotZero(len(profiler.newStacks))
	assert.NotZero(len(profiler.routines))
	assert.NotZero(countSamples(profiler))
	assert.Equal(ringBufferSize, profiler.samplesBucketsHead.Len())

	framesLen := len(profiler.frames)
	frameIndexesLen := len(profiler.frameIndexes)
	stacksLen := len(profiler.stacks)
	stackIndexesLen := len(profiler.stackIndexes)
	routinesLen := len(profiler.routines)
	samplesLen := countSamples(profiler)

	// On another tick, we will have the same data plus one frame and stack representing the profiler.onTick() call on the next line.
	profiler.onTick()
	assert.Equal(framesLen+1, len(profiler.frames))
	assert.Equal(frameIndexesLen+1, len(profiler.frameIndexes))
	assert.Equal(1, len(profiler.newFrames))
	assert.Equal(stacksLen+1, len(profiler.stacks))
	assert.Equal(stackIndexesLen+1, len(profiler.stackIndexes))
	assert.Equal(1, len(profiler.newStacks))
	assert.Equal(routinesLen, len(profiler.routines))
	assert.Equal(samplesLen*2, countSamples(profiler))
	assert.Equal(ringBufferSize, profiler.samplesBucketsHead.Len())

	// On another tick, we will have the same data plus one frame and stack representing the profiler.onTick() call on the next line.
	profiler.onTick()
	assert.Equal(framesLen+2, len(profiler.frames))
	assert.Equal(frameIndexesLen+2, len(profiler.frameIndexes))
	assert.Equal(1, len(profiler.newFrames))
	assert.Equal(stacksLen+2, len(profiler.stacks))
	assert.Equal(stackIndexesLen+2, len(profiler.stackIndexes))
	assert.Equal(1, len(profiler.newStacks))
	assert.Equal(routinesLen, len(profiler.routines))
	assert.Equal(samplesLen*3, countSamples(profiler))
	assert.Equal(ringBufferSize, profiler.samplesBucketsHead.Len())
}

func testTick(t *testing.T, count, i int, prevTick time.Time) time.Time {
	var sinceLastTick = time.Since(prevTick).Microseconds()
	t.Logf("tick %2d/%d after %d μs", i+1, count, sinceLastTick)
	return time.Now()
}

func isCI() bool {
	return os.Getenv("CI") != ""
}

// This test measures the accuracy of time.NewTicker() on the current system.
func TestProfilerTimeTicker(t *testing.T) {
	if isCI() {
		t.Skip("Skipping on CI because the machines are too overloaded to provide consistent ticker resolution.")
	}

	onProfilerStart() // This fixes Windows ticker resolution.

	t.Logf("We're expecting a tick once every %d μs", profilerSamplingRate.Microseconds())

	var startTime = time.Now()
	var ticker = time.NewTicker(profilerSamplingRate)
	defer ticker.Stop()

	// wait until 10 ticks have passed
	var count = 10
	var prevTick = time.Now()
	for i := 0; i < count; i++ {
		<-ticker.C
		prevTick = testTick(t, count, i, prevTick)
	}

	var elapsed = time.Since(startTime)
	require.LessOrEqual(t, elapsed.Microseconds(), profilerSamplingRate.Microseconds()*int64(count+3))
}

// This test measures the accuracy of time.Sleep() on the current system.
func TestProfilerTimeSleep(t *testing.T) {
	t.Skip("This test isn't necessary at the moment because we don't use time.Sleep() in the profiler.")

	onProfilerStart() // This fixes Windows ticker resolution.

	t.Logf("We're expecting a tick once every %d μs", profilerSamplingRate.Microseconds())

	var startTime = time.Now()

	// wait until 10 ticks have passed
	var count = 10
	var prevTick = time.Now()
	var next = time.Now()
	for i := 0; i < count; i++ {
		next = next.Add(profilerSamplingRate)
		time.Sleep(time.Until(next))
		prevTick = testTick(t, count, i, prevTick)
	}

	var elapsed = time.Since(startTime)
	require.LessOrEqual(t, elapsed.Microseconds(), profilerSamplingRate.Microseconds()*int64(count+3))
}

// Benchmark results (run without executing which mess up results)
// $ go test -run=^$ -bench "BenchmarkProfiler*"
//
// goos: windows
// goarch: amd64
// pkg: github.com/getsentry/sentry-go
// cpu: 12th Gen Intel(R) Core(TM) i7-12700K
// BenchmarkProfilerStartStop-20                      38008             31072 ns/op           20980 B/op        108 allocs/op
// BenchmarkProfilerOnTick-20                         65700             18065 ns/op             260 B/op          4 allocs/op
// BenchmarkProfilerCollect-20                        67063             16907 ns/op               0 B/op          0 allocs/op
// BenchmarkProfilerProcess-20                      2296788               512.9 ns/op           268 B/op          4 allocs/op
// BenchmarkProfilerOverheadBaseline-20                 192           6250525 ns/op
// BenchmarkProfilerOverheadWithProfiler-20             187           6249490 ns/op

func BenchmarkProfilerStartStop(b *testing.B) {
	var bench = func(name string, wait bool) {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				startProfiling(time.Now()).Stop(wait)
			}
		})
	}

	bench("Wait", true)
	bench("NoWait", false)
}

func BenchmarkProfilerOnTick(b *testing.B) {
	profiler := newProfiler(time.Now())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		profiler.onTick()
	}
}

func BenchmarkProfilerCollect(b *testing.B) {
	profiler := newProfiler(time.Now())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = profiler.collectRecords()
	}
}

func BenchmarkProfilerProcess(b *testing.B) {
	profiler := newProfiler(time.Now())
	records := profiler.collectRecords()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		profiler.processRecords(uint64(i), records)
	}
}

func profilerBenchmark(t *testing.T, b *testing.B, withProfiling bool, arg int) {
	var p profiler
	if withProfiling {
		p = startProfiling(time.Now())
	}
	b.ResetTimer()

	var wg sync.WaitGroup
	wg.Add(b.N)
	for i := 0; i < b.N; i++ {
		go func() {
			start := time.Now()
			_ = findPrimeNumber(arg)
			end := time.Now()
			if p != nil {
				_ = p.GetSlice(start, end)
			}
			wg.Done()
		}()
	}
	wg.Wait()

	b.StopTimer()
	if p != nil {
		p.Stop(true)
		// Let's captured data so we can see what has been profiled if there's an error.
		// Previously, there have been tests that have started (and left running) global Sentry instance and goroutines.
		t.Logf("Profiler captured %d goroutines.", len(p.(*profileRecorder).routines))
		t.Log("Captured frames related to the profiler benchmark:")
		isRelatedToProfilerBenchmark := func(f *Frame) bool {
			return strings.Contains(f.AbsPath, "profiler") || strings.Contains(f.AbsPath, "benchmark.go") || strings.Contains(f.AbsPath, "testing.go")
		}
		for _, frame := range p.(*profileRecorder).frames {
			if isRelatedToProfilerBenchmark(frame) {
				t.Logf("%s %s\tat %s:%d", frame.Module, frame.Function, frame.AbsPath, frame.Lineno)
			}
		}
		t.Log(strings.Repeat("-", 80))
		t.Log("Unknown frames (these may be a cause of high overhead):")
		for _, frame := range p.(*profileRecorder).frames {
			if !isRelatedToProfilerBenchmark(frame) {
				t.Logf("%s %s\tat %s:%d", frame.Module, frame.Function, frame.AbsPath, frame.Lineno)
			}
		}
		t.Log(strings.Repeat("=", 80))
	}
}

func TestProfilerOverhead(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping overhead benchmark in short mode.")
	}
	if isCI() {
		t.Skip("Skipping on CI because the machines are too overloaded to run the test properly - they show between 3 and 30 %% overhead....")
	}

	// first, find large-enough argument so that findPrimeNumber(arg) takes more than 100ms
	var arg = 10000
	for {
		start := time.Now()
		_ = findPrimeNumber(arg)
		end := time.Now()
		if end.Sub(start) > 100*time.Millisecond {
			t.Logf("Found arg = %d that takes %d ms to process.", arg, end.Sub(start).Milliseconds())
			break
		}
		arg += 10000
	}

	var assert = assert.New(t)
	var baseline = testing.Benchmark(func(b *testing.B) { profilerBenchmark(t, b, false, arg) })
	var profiling = testing.Benchmark(func(b *testing.B) { profilerBenchmark(t, b, true, arg) })

	t.Logf("Without profiling: %v\n", baseline.String())
	t.Logf("With profiling:    %v\n", profiling.String())

	var overhead = float64(profiling.NsPerOp())/float64(baseline.NsPerOp())*100 - 100
	var maxOverhead = 5.0
	t.Logf("Profiling overhead: %f percent\n", overhead)
	assert.Less(overhead, maxOverhead)
}