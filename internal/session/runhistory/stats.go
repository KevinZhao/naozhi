package runhistory

import "sort"

// ComputeStats aggregates runs into a SessionRunStats without mutating the
// input. An empty input yields the zero value.
func ComputeStats(runs []SessionRun) SessionRunStats {
	var st SessionRunStats
	st.Count = len(runs)
	if st.Count == 0 {
		return st
	}

	durs := make([]int64, 0, len(runs))
	for i := range runs {
		d := runs[i].DurationMS
		durs = append(durs, d)
		st.TotalMS += d
		st.TotalCostUSD += runs[i].CostUSD
		if d > st.MaxMS {
			st.MaxMS = d
		}
		switch runs[i].Outcome {
		case OutcomeCompleted:
			st.CompletedCnt++
		case OutcomeError:
			st.ErrorCnt++
		case OutcomeTimeout:
			st.TimeoutCnt++
		}
	}
	st.AvgMS = st.TotalMS / int64(st.Count)

	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	st.P50MS = percentile(durs, 50)
	return st
}

// percentile returns the nearest-rank p-th percentile of a sorted, non-empty slice.
func percentile(sorted []int64, p int) int64 {
	n := len(sorted)
	if n == 1 {
		return sorted[0]
	}
	rank := (p*n + 99) / 100
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}
