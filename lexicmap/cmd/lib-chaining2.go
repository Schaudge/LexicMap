// Copyright © 2023-2024 Wei Shen <shenwei356@gmail.com>
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package cmd

import (
	"math"
	"sync"
)

// Chaining2Options contains all options in chaining.
type Chaining2Options struct {
	MaxGap   int
	MinScore int // minimum score of a chain

	// only used in Chain2
	MaxDistance int
	Band        int // only check i in range of  i − A < j < i
}

// DefaultChaining2Options is the defalt vaule of Chaining2Option.
var DefaultChaining2Options = Chaining2Options{
	MaxGap:   32,
	MinScore: 20,

	MaxDistance: 50,
	Band:        20,
}

// Chainer2 is an object for chaining the anchors in two similar sequences.
// Different from Chainer, Chainer2 find chains with no overlaps.
// Anchors/seeds/substrings in Chainer2 is denser than those in Chainer,
// and the chaining score function is also much simpler, only considering
// the lengths of anchors and gaps between them.
type Chainer2 struct {
	options *Chaining2Options

	// scores        []int
	maxscores     []int
	maxscoresIdxs []int

	bounds []int // 4 * chains
}

// NewChainer creates a new chainer.
func NewChainer2(options *Chaining2Options) *Chainer2 {
	c := &Chainer2{
		options: options,

		// scores:        make([]int, 0, 10240),
		maxscores:     make([]int, 0, 10240),
		maxscoresIdxs: make([]int, 0, 10240),
		bounds:        make([]int, 32),
	}
	return c
}

// RecycleChainingResult reycles the chaining paths.
// Please remember to call this after using the results.
func RecycleChaining2Result(chains *[]*Chain2Result) {
	for _, chain := range *chains {
		poolChain2.Put(chain)
	}
	poolChains2.Put(chains)
}

var poolChains2 = &sync.Pool{New: func() interface{} {
	tmp := make([]*Chain2Result, 0, 32)
	return &tmp
}}

var poolChain2 = &sync.Pool{New: func() interface{} {
	return &Chain2Result{
		Chain: make([]int, 0, 1024),
	}
}}

// Chain2Result represents a result of a chain
type Chain2Result struct {
	Chain        []int // Paths.
	MatchedBases int   // The number of matched bases.
	AlignedBases int   // The number of aligned bases.
	QBegin, QEnd int   // Query begin/end position (0-based)
	TBegin, TEnd int   // Target begin/end position (0-based)
}

func (r *Chain2Result) Reset() {
	r.Chain = r.Chain[:0]
}

// Chain finds the possible chain paths.
// Please remember to call RecycleChainingResult after using the results.
// Returned results:
//  1. Chain2Results.
//  2. The number of matched bases.
//  3. The number of aligned bases.
func (ce *Chainer2) Chain(subs *[]*SubstrPair) (*[]*Chain2Result, int, int, int, int, int, int) {
	n := len(*subs)

	if n == 1 { // for one seed, just check the seed weight
		paths := poolChains2.Get().(*[]*Chain2Result)
		*paths = (*paths)[:0]

		sub := (*subs)[0]
		if sub.Len >= ce.options.MinScore { // the length of anchor
			path := poolChain2.Get().(*Chain2Result)
			path.Reset()

			path.Chain = append(path.Chain, 0)

			*paths = append(*paths, path)

			// TODO: compute qb, qe, tb, te. though it's unnecessary
			return paths, sub.Len, sub.Len, 0, 0, 0, 0
		}

		return paths, 0, 0, 0, 0, 0, 0
	}

	var i, _b, j, k int
	band := ce.options.Band // band size of banded-DP

	// a list for storing score matrix, the size is band * len(seeds pair)
	// scores := ce.scores[:0]
	// size := n * (band + 1)
	// for k = 0; k < size; k++ {
	// 	scores = append(scores, 0)
	// }

	// reused objects

	// the maximum score for each seed, the size is n
	maxscores := ce.maxscores
	maxscores = maxscores[:0]
	// index of previous seed, the size is n. pointers for backtracking.
	maxscoresIdxs := ce.maxscoresIdxs
	maxscoresIdxs = maxscoresIdxs[:0]

	// initialize
	maxscores = append(maxscores, (*subs)[0].Len)
	maxscoresIdxs = append(maxscoresIdxs, 0)

	// compute scores
	var s, m, M, d, g int
	var mj, Mi int
	var a, b *SubstrPair
	maxGap := ce.options.MaxGap
	maxDistance := ce.options.MaxDistance
	// scores[0] = (*subs)[0].Len
	for i = 1; i < n; i++ {
		a = (*subs)[i] // current seed/anchor
		k = band * i   // index of current seed in the score matrix

		// just initialize the max score, which comes from the current seed
		m, mj = a.Len, i
		// scores[k] = m

		for _b = 1; _b <= band; _b++ { // check previous $band seeds
			j = i - _b // index of the previous seed
			if j < 0 {
				break
			}

			b = (*subs)[j] // previous seed/anchor
			k++            // index of previous seed in the score matrix

			if b.TBegin > a.TBegin { // filter out messed/crossed anchors
				continue
			}

			d = distance2(a, b)
			if d > maxDistance { // limit the distance. necessary?
				continue
			}

			g = gap2(a, b)
			if g > maxGap { // limit the gap. necessary?
				continue
			}

			s = maxscores[j] + b.Len - g // compute the score
			// scores[k] = s                // necessary?

			if s >= m { // update the max score of current seed/anchor
				m = s
				mj = j
			}
		}

		maxscores = append(maxscores, m)          // save the max score of the whole
		maxscoresIdxs = append(maxscoresIdxs, mj) // save where the max score comes from

		if m > M { // the biggest score in the whole score matrix
			M, Mi = m, i
		}
	}

	// print the score matrix
	// fmt.Printf("i\tpair-i\tiMax\tj:scores\n")
	// for i = 0; i < n; i++ {
	// 	fmt.Printf("%d\t%s\t%d", i, (*subs)[i], maxscoresIdxs[i])
	// 	// k = i * band
	// 	// for _b = 0; _b <= band; _b++ {
	// 	// 	if i-_b >= 0 {
	// 	// 		fmt.Printf("\t%3d:%-4d", i-_b, scores[k])
	// 	// 	}

	// 	// 	k++
	// 	// }
	// 	fmt.Printf("\n")
	// }

	// backtrack

	paths := poolChains2.Get().(*[]*Chain2Result)
	*paths = (*paths)[:0]

	// check the highest score, for early quit,
	// but what's the number?
	if M < 100 {
		return paths, 0, 0, 0, 0, 0, 0
	}

	var nMatchedBases, nAlignedBases int
	minScore := ce.options.MinScore
	bounds := ce.bounds[:0]

	_, qB, qE, tB, tE := chainARegion(
		subs,
		maxscores,
		maxscoresIdxs,
		0,
		minScore,
		paths,
		&nMatchedBases,
		&nAlignedBases,
		Mi,
		&bounds,
	)

	return paths, nMatchedBases, nAlignedBases, qB, qE, tB, tE
}

func chainARegion(subs *[]*SubstrPair, // a region of the subs
	maxscores []int, // a region of maxscores
	maxscoresIdxs []int,
	offset int, // offset of this region of subs
	minScore int, // the threshold
	paths *[]*Chain2Result, // paths
	_nMatchedBases *int,
	_nAlignedBases *int,
	Mi0 int, // found Mi
	bounds *[]int, // intervals of previous chains
) (
	int, // score
	int, // query begin position (0-based)
	int, // query end position (0-based)
	int, // target begin position (0-based)
	int, // target end position (0-based)
) {
	// fmt.Printf("region: [%d, %d]\n", offset, offset+len(*subs)-1)
	var m, M int
	var i, Mi int
	if Mi0 < 0 { // Mi is not given
		// find the next highest score
		for i, m = range maxscores {
			if m > M {
				M, Mi = m, i
			}
		}
		if M < minScore { // no valid anchors
			return 0, -1, -1, -1, -1
		}
	} else {
		Mi = Mi0
	}
	// fmt.Printf("  Mi: %d, M: %d\n", Mi, M)

	var nMatchedBases int
	var nAlignedBases int

	i = Mi
	var j int
	var qB, qE, tB, tE int // the bound of the chain (0-based)
	qB, tB = math.MaxInt, math.MaxInt
	var qb, qe, tb, te int // the bound (0-based)
	var sub *SubstrPair
	var beginOfNextAnchor int
	var overlapped bool
	var nb, bi, bj int // index of bounds
	firstAnchorOfAChain := true
	path := poolChain2.Get().(*Chain2Result)
	path.Reset()
	for {
		j = maxscoresIdxs[i] - offset // previous seed

		if j < 0 { // the first anchor is not in current region
			break
		}

		// check if an anchor overlaps with previous chains
		//
		// Query
		// |        te  / (OK)
		// |        |  /
		// |(NO)/   |____qe
		// |   /   /
		// |qb____/    / (NO)
		// |   /  |   /
		// |OK/   |tb
		// o-------------------- Ref
		//
		sub = (*subs)[i]
		overlapped = false
		nb = len(*bounds) >> 2 // len(bounds) / 4
		for bi = 0; bi < nb; bi++ {
			bj = bi << 2
			if !((sub.QBegin > (*bounds)[bj+1] && sub.TBegin > (*bounds)[bj+3]) || // top right
				(sub.QBegin+sub.Len-1 < (*bounds)[bj] && sub.TBegin+sub.Len-1 < (*bounds)[bj+2])) { // bottom left
				overlapped = true
				break
			}
		}

		if overlapped {
			// fmt.Printf("  %d (%s) is overlapped previous chain, j=%d\n", i, *sub, j)

			// can not continue here, must check if i == j
		} else {
			path.Chain = append(path.Chain, i+offset) // record the seed

			// fmt.Printf(" AAADDD %d (%s). firstAnchorOfAChain: %v\n", i, *sub, firstAnchorOfAChain)

			if firstAnchorOfAChain {
				// fmt.Printf(" record bound beginning with: %s\n", sub)
				firstAnchorOfAChain = false

				qe = sub.QBegin + sub.Len - 1   // end
				te = sub.TBegin + sub.Len - 1   // end
				qb, tb = sub.QBegin, sub.TBegin // in case there's only one anchor

				nMatchedBases += sub.Len
			} else {
				qb, tb = sub.QBegin, sub.TBegin // begin

				if sub.QBegin+sub.Len-1 >= beginOfNextAnchor {
					nMatchedBases += beginOfNextAnchor - sub.QBegin
				} else {
					nMatchedBases += sub.Len
				}
			}
			beginOfNextAnchor = sub.QBegin
		}

		if i == j { // the path starts here
			if firstAnchorOfAChain { // sadly, there's no anchor added.
				break
			}

			nAlignedBases += qe - qb + 1

			reverseInts(path.Chain)
			path.AlignedBases = nAlignedBases
			path.MatchedBases = nMatchedBases
			path.QBegin, path.QEnd = qb, qe
			path.TBegin, path.TEnd = tb, te
			*paths = append(*paths, path)

			*_nAlignedBases += nAlignedBases
			*_nMatchedBases += nMatchedBases

			// fmt.Printf("chain %d (%d, %d) vs (%d, %d), a:%d, m:%d\n",
			// 	len(*paths), qb, qe, tb, te, nAlignedBases, nMatchedBases)

			firstAnchorOfAChain = true
			break
		}

		i = j
	}

	if j < 0 { // the first anchor is not in current region
		// fmt.Printf(" found only part of the chain, nAnchors: %d\n", len(*path))
		if len(path.Chain) == 0 {
			poolChain.Put(path)
		} else {
			nAlignedBases += qe - qb + 1

			reverseInts(path.Chain)
			path.AlignedBases = nAlignedBases
			path.MatchedBases = nMatchedBases
			path.QBegin, path.QEnd = qb, qe
			path.TBegin, path.TEnd = tb, te
			*paths = append(*paths, path)

			*_nAlignedBases += nAlignedBases
			*_nMatchedBases += nMatchedBases

			// fmt.Printf("chain %d (%d, %d) vs (%d, %d), a:%d, m:%d\n",
			// 	len(*paths), qb, qe, tb, te, nAlignedBases, nMatchedBases)
		}
	}

	*bounds = append(*bounds, qb)
	*bounds = append(*bounds, qe)
	*bounds = append(*bounds, tb)
	*bounds = append(*bounds, te)

	// initialize the boundary
	qB, qE = qb, qe
	tB, tE = tb, te

	// fmt.Printf("  i: %d\n", i)

	// the unchecked region on the right
	if Mi != len(maxscores)-1 { // Mi is not the last element
		tmp := (*subs)[Mi+1:]
		_score, _qB, _qE, _tB, _tE := chainARegion(
			&tmp,
			maxscores[Mi+1:],
			maxscoresIdxs[Mi+1:],
			offset+Mi+1,
			minScore,
			paths,
			_nMatchedBases,
			_nAlignedBases,
			-1,
			bounds,
		)
		if _score > 0 {
			if _qB < qB {
				qB = _qB
			}
			if _qE > qE {
				qE = _qE
			}
			if _tB < tB {
				tB = _tB
			}
			if _tE > tE {
				tE = _tE
			}
		}
	}

	// the unchecked region on the left
	if i > 0 { // the first anchor is not the first element
		tmp := (*subs)[:i]
		_score, _qB, _qE, _tB, _tE := chainARegion(
			&tmp,
			maxscores[:i],
			maxscoresIdxs[:i],
			offset,
			minScore,
			paths,
			_nMatchedBases,
			_nAlignedBases,
			-1,
			bounds,
		)
		if _score > 0 {
			if _qB < qB {
				qB = _qB
			}
			if _qE > qE {
				qE = _qE
			}
			if _tB < tB {
				tB = _tB
			}
			if _tE > tE {
				tE = _tE
			}
		}
	}

	return M, qB, qE, tB, tE
}

func distance2(a, b *SubstrPair) int {
	q := a.QBegin - b.QBegin
	t := a.TBegin - b.TBegin
	if q > t {
		return q
	}
	return t
}

func gap2(a, b *SubstrPair) int {
	g := a.QBegin - b.QBegin - (a.TBegin - b.TBegin)
	if g < 0 {
		return -g
	}
	return g
}
