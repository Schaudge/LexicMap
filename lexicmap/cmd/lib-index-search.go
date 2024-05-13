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
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"

	"github.com/shenwei356/LexicMap/lexicmap/cmd/genome"
	"github.com/shenwei356/LexicMap/lexicmap/cmd/kv"
	"github.com/shenwei356/lexichash"
	"github.com/shenwei356/util/pathutil"
)

// IndexSearchingOptions contains all options for searching
type IndexSearchingOptions struct {
	// general
	NumCPUs      int
	Verbose      bool // show log
	Log2File     bool // log file
	MaxOpenFiles int  // maximum opened files, used in merging indexes

	// seed searching
	InMemorySearch  bool  // load the seed/kv data into memory
	MinPrefix       uint8 // minimum prefix length, e.g., 15
	MaxMismatch     int   // maximum mismatch, e.g., 3
	MinSinglePrefix uint8 // minimum prefix length of the single seed, e.g., 20
	TopN            int   // keep the topN scores, e.g, 10

	// seeds chaining
	MaxGap      float64 // e.g., 5000
	MaxDistance float64 // e.g., 20k

	// alignment
	ExtendLength int // the length of extra sequence on the flanking of seeds.
	// seq similarity
	MinQueryAlignedFractionInAGenome float64 // minimum query aligned fraction in the target genome

	// Output
	OutputSeq bool
}

func CheckIndexSearchingOptions(opt *IndexSearchingOptions) error {
	if opt.NumCPUs < 1 {
		return fmt.Errorf("invalid number of CPUs: %d, should be >= 1", opt.NumCPUs)
	}
	if opt.MaxOpenFiles < 2 {
		return fmt.Errorf("invalid max open files: %d, should be >= 2", opt.MaxOpenFiles)
	}

	// ------------------------
	if opt.MinPrefix < 3 || opt.MinPrefix > 32 {
		return fmt.Errorf("invalid MinPrefix: %d, valid range: [3, 32]", opt.MinPrefix)
	}

	return nil
}

var DefaultIndexSearchingOptions = IndexSearchingOptions{
	NumCPUs:      runtime.NumCPU(),
	MaxOpenFiles: 512,

	MinPrefix:       15,
	MaxMismatch:     -1,
	MinSinglePrefix: 20,
	TopN:            500,

	MaxGap:      5000,
	MaxDistance: 10000,

	ExtendLength:                     2000,
	MinQueryAlignedFractionInAGenome: 70,
}

// Index creates a LexicMap index from a path
// and supports searching with query sequences.
type Index struct {
	path string

	openFileTokens chan int // control the max open files

	// lexichash
	lh *lexichash.LexicHash
	k  int
	k8 uint8

	// k-mer-value searchers
	Searchers         []*kv.Searcher
	InMemorySearchers []*kv.InMemorySearcher
	searcherTokens    []chan int // make sure one seachers is only used by one query

	// general options, and some for seed searching
	opt *IndexSearchingOptions

	// for seed chaining
	chainingOptions *ChainingOptions
	poolChainers    *sync.Pool

	// for sequence comparing
	contigInterval    int // read from info file
	seqCompareOption  *SeqComparatorOptions
	poolSeqComparator *sync.Pool
	poolChainers2     *sync.Pool

	// genome data reader
	poolGenomeRdrs []chan *genome.Reader
	hasGenomeRdrs  bool
}

// SetSeqCompareOptions sets the sequence comparing options
func (idx *Index) SetSeqCompareOptions(sco *SeqComparatorOptions) {
	idx.seqCompareOption = sco
	idx.poolChainers2 = &sync.Pool{New: func() interface{} {
		return NewChainer2(&sco.Chaining2Options)
	}}
	idx.poolSeqComparator = &sync.Pool{New: func() interface{} {
		return NewSeqComparator(sco, idx.poolChainers2)
	}}
}

// NewIndexSearcher creates a new searcher
func NewIndexSearcher(outDir string, opt *IndexSearchingOptions) (*Index, error) {
	ok, err := pathutil.DirExists(outDir)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("index path not found: %s", outDir)
	}

	idx := &Index{path: outDir, opt: opt}

	// -----------------------------------------------------
	// info file
	fileInfo := filepath.Join(outDir, FileInfo)
	info, err := readIndexInfo(fileInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to read info file: %s", err)
	}
	if info.MainVersion != MainVersion {
		checkError(fmt.Errorf("index main versions do not match: %d (index) != %d (tool). please re-create the index", info.MainVersion, MainVersion))
	}

	if idx.opt.MaxOpenFiles < info.Chunks+2 {
		return nil, fmt.Errorf("max open files (%d) should not be < chunks (%d) + 2",
			idx.opt.MaxOpenFiles, info.Chunks)
	}

	idx.contigInterval = info.ContigInterval

	// -----------------------------------------------------
	// read masks
	fileMask := filepath.Join(outDir, FileMasks)
	if opt.Verbose || opt.Log2File {
		log.Infof("  reading masks...")
	}
	idx.lh, err = lexichash.NewFromFile(fileMask)
	if err != nil {
		return nil, err
	}

	// create a lookup table for faster masking
	lenPrefix := 1
	for 1<<(lenPrefix<<1) <= len(idx.lh.Masks) {
		lenPrefix++
	}
	lenPrefix--
	err = idx.lh.IndexMasks(lenPrefix)
	if err != nil {
		return nil, err
	}

	idx.k8 = uint8(idx.lh.K)
	idx.k = idx.lh.K

	if opt.MinPrefix > idx.k8 { // check again
		return nil, fmt.Errorf("MinPrefix (%d) should not be <= k (%d)", opt.MinPrefix, idx.k8)
	}

	// -----------------------------------------------------
	// read index of seeds

	inMemorySearch := idx.opt.InMemorySearch

	threads := opt.NumCPUs
	dirSeeds := filepath.Join(outDir, DirSeeds)
	fileSeeds := make([]string, 0, 64)
	fs.WalkDir(os.DirFS(dirSeeds), ".", func(p string, d fs.DirEntry, err error) error {
		if filepath.Ext(p) == ExtSeeds {
			fileSeeds = append(fileSeeds, filepath.Join(dirSeeds, p))
		}
		return nil
	})

	if len(fileSeeds) == 0 {
		return nil, fmt.Errorf("seeds file not found in: %s", dirSeeds)
	}
	if inMemorySearch {
		idx.InMemorySearchers = make([]*kv.InMemorySearcher, 0, len(fileSeeds))
	} else {
		idx.Searchers = make([]*kv.Searcher, 0, len(fileSeeds))
	}
	idx.searcherTokens = make([]chan int, len(fileSeeds))
	for i := range idx.searcherTokens {
		idx.searcherTokens[i] = make(chan int, 1)
	}

	// check options again
	if opt.MaxOpenFiles < len(fileSeeds) {
		return nil, fmt.Errorf("MaxOpenFiles (%d) should be > number of seeds files (%d), or even bigger", opt.MaxOpenFiles, len(fileSeeds))
	}
	idx.openFileTokens = make(chan int, opt.MaxOpenFiles) // tokens

	// read indexes

	if opt.Verbose || opt.Log2File {
		if inMemorySearch {
			log.Infof("  reading seeds (k-mer-value) data into memory...")
		} else {
			log.Infof("  reading indexes of seeds (k-mer-value) data...")
		}
	}
	done := make(chan int)
	var ch chan *kv.Searcher
	var chIM chan *kv.InMemorySearcher

	if inMemorySearch {
		chIM = make(chan *kv.InMemorySearcher, threads)
		go func() {
			for scr := range chIM {
				idx.InMemorySearchers = append(idx.InMemorySearchers, scr)
			}
			done <- 1
		}()
	} else {
		ch = make(chan *kv.Searcher, threads)
		go func() {
			for scr := range ch {
				idx.Searchers = append(idx.Searchers, scr)

				idx.openFileTokens <- 1 // increase the number of open files
			}
			done <- 1
		}()
	}
	var wg sync.WaitGroup
	tokens := make(chan int, threads)
	for _, file := range fileSeeds {
		wg.Add(1)
		tokens <- 1
		go func(file string) {
			if inMemorySearch { // read all the k-mer-value data into memory
				scr, err := kv.NewInMemomrySearcher(file)
				if err != nil {
					checkError(fmt.Errorf("failed to create a in-memory searcher from file: %s: %s", file, err))
				}

				chIM <- scr
			} else { // just read the index data
				scr, err := kv.NewSearcher(file)
				if err != nil {
					checkError(fmt.Errorf("failed to create a searcher from file: %s: %s", file, err))
				}

				ch <- scr
			}

			wg.Done()
			<-tokens
		}(file)
	}
	wg.Wait()
	if inMemorySearch {
		close(chIM)
	} else {
		close(ch)
	}
	<-done

	// we can create genome reader pools
	n := (idx.opt.MaxOpenFiles - len(fileSeeds)) / info.GenomeBatches
	if n < 2 {
	} else {
		n >>= 1
		if n > opt.NumCPUs {
			n = opt.NumCPUs
		}
		if opt.Verbose || opt.Log2File {
			log.Infof("  creating genome reader pools, each batch with %d readers...", n)
		}
		idx.poolGenomeRdrs = make([]chan *genome.Reader, info.GenomeBatches)
		for i := 0; i < info.GenomeBatches; i++ {
			idx.poolGenomeRdrs[i] = make(chan *genome.Reader, n)
		}

		// parallelize it
		var wg sync.WaitGroup
		tokens := make(chan int, opt.NumCPUs)
		for i := 0; i < info.GenomeBatches; i++ {
			for j := 0; j < n; j++ {
				tokens <- 1
				wg.Add(1)
				go func(i int) {
					fileGenomes := filepath.Join(outDir, DirGenomes, batchDir(i), FileGenomes)
					rdr, err := genome.NewReader(fileGenomes)
					if err != nil {
						checkError(fmt.Errorf("failed to create genome reader: %s", err))
					}
					idx.poolGenomeRdrs[i] <- rdr

					idx.openFileTokens <- 1 // genome file

					wg.Done()
					<-tokens
				}(i)
			}
		}
		wg.Wait()

		idx.hasGenomeRdrs = true
	}

	// other resources
	co := &ChainingOptions{
		MaxGap:      opt.MaxGap,
		MinScore:    seedWeight(float64(opt.MinSinglePrefix)),
		MaxDistance: opt.MaxDistance,
	}
	idx.chainingOptions = co
	idx.poolChainers = &sync.Pool{New: func() interface{} {
		return NewChainer(co)
	}}

	return idx, nil
}

// Close closes the searcher.
func (idx *Index) Close() error {
	var _err error

	// seed data
	if idx.opt.InMemorySearch {
		for _, scr := range idx.InMemorySearchers {
			err := scr.Close()
			if err != nil {
				_err = err
			}
		}
	} else {
		for _, scr := range idx.Searchers {
			err := scr.Close()
			if err != nil {
				_err = err
			}
		}
	}

	// genome reader
	if idx.hasGenomeRdrs {
		var wg sync.WaitGroup
		for _, pool := range idx.poolGenomeRdrs {
			wg.Add(1)
			go func(pool chan *genome.Reader) {
				close(pool)
				for rdr := range pool {
					err := rdr.Close()
					if err != nil {
						_err = err
					}
				}
				wg.Done()
			}(pool)
		}
		wg.Wait()
	}
	return _err
}

// --------------------------------------------------------------------------
// structs for seeding results

// SubstrPair represents a pair of found substrings/seeds, it's also called an anchor.
type SubstrPair struct {
	QBegin int32 // start position of the substring (0-based) in query
	TBegin int32 // start position of the substring (0-based) in reference

	// Code   uint64 // k-mer, only for debugging

	Len      uint8 // prefix length
	Mismatch uint8 // number of mismatches

	TRC bool // is the substring from the reference seq on the negative strand.
	QRC bool // is the substring from the query seq on the negative strand.
}

func (s SubstrPair) String() string {
	s1 := "+"
	s2 := "+"
	if s.QRC {
		s1 = "-"
	}
	if s.TRC {
		s2 = "-"
	}
	return fmt.Sprintf("%3d-%3d (%s) vs %3d-%3d (%s), len:%2d, mismatches:%d",
		s.QBegin+1, s.QBegin+int32(s.Len), s1, s.TBegin+1, s.TBegin+int32(s.Len), s2, s.Len, s.Mismatch)
}

var poolSub = &sync.Pool{New: func() interface{} {
	return &SubstrPair{}
}}

var poolSubs = &sync.Pool{New: func() interface{} {
	tmp := make([]*SubstrPair, 0, 256)
	return &tmp
}}

// RecycleSubstrPairs recycles a list of SubstrPairs
func RecycleSubstrPairs(subs *[]*SubstrPair) {
	for _, sub := range *subs {
		poolSub.Put(sub)
	}
	poolSubs.Put(subs)
}

// ClearSubstrPairs removes nested/embedded and same anchors. k is the largest k-mer size.
func ClearSubstrPairs(subs *[]*SubstrPair, k int) {
	if len(*subs) < 2 {
		return
	}

	// sort substrings/seeds in ascending order based on the starting position
	// and in descending order based on the ending position.
	sort.Slice(*subs, func(i, j int) bool {
		a := (*subs)[i]
		b := (*subs)[j]
		if a.QBegin == b.QBegin {
			// return a.QBegin+int32(a.Len) >= b.QBegin+int32(b.Len)
			if a.QBegin+int32(a.Len) == b.QBegin+int32(b.Len) {
				return a.TBegin <= b.TBegin
			}
			return a.QBegin+int32(a.Len) > b.QBegin+int32(b.Len)
		}
		return a.QBegin < b.QBegin
	})

	var p *SubstrPair
	var upbound, vQEnd, vTEnd int32
	var j int
	markers := poolBoolList.Get().(*[]bool)
	*markers = (*markers)[:0]
	for range *subs {
		*markers = append(*markers, false)
	}
	for i, v := range (*subs)[1:] {
		vQEnd = int32(v.QBegin) + int32(v.Len)
		upbound = int32(vQEnd) - int32(k)
		vTEnd = int32(v.TBegin) + int32(v.Len)
		j = i
		for j >= 0 { // have to check previous N seeds
			p = (*subs)[j]
			if p.QBegin < upbound { // no need to check
				break
			}

			// same or nested region
			if vQEnd <= p.QBegin+int32(p.Len) &&
				v.TBegin >= p.TBegin && vTEnd <= p.TBegin+int32(p.Len) {
				poolSub.Put(v)         // do not forget to recycle the object
				(*markers)[i+1] = true // because of: range (*subs)[1:]
				break
			}

			j--
		}
	}

	j = 0
	for i, embedded := range *markers {
		if !embedded {
			(*subs)[j] = (*subs)[i]
			j++
		}
	}
	if j > 0 {
		*subs = (*subs)[:j]
	}

	poolBoolList.Put(markers)
}

var poolBoolList = &sync.Pool{New: func() interface{} {
	m := make([]bool, 0, 1024)
	return &m
}}

// --------------------------------------------------------------------------
// structs for searching result

var poolSimilarityDetail = &sync.Pool{New: func() interface{} {
	return &SimilarityDetail{
		SeqID: make([]byte, 0, 128),
	}
}}

var poolSimilarityDetails = &sync.Pool{New: func() interface{} {
	tmp := make([]*SimilarityDetail, 0, 8)
	return &tmp
}}

var poolSearchResult = &sync.Pool{New: func() interface{} {
	return &SearchResult{
		ID: make([]byte, 0, 128),
	}
}}

var poolSearchResults = &sync.Pool{New: func() interface{} {
	tmp := make([]*SearchResult, 0, 16)
	return &tmp
}}

// SearchResult stores a search result for the given query sequence.
type SearchResult struct {
	GenomeBatch int
	GenomeIndex int
	ID          []byte
	GenomeSize  int

	Subs *[]*SubstrPair // matched substring pairs (query,target)

	Score  float64 //  score for soring
	Chains *[]*[]int

	// more about the alignment detail
	SimilarityDetails *[]*SimilarityDetail // sequence comparing
	AlignedFraction   float64              // query coverage per genome
}

// SimilarityDetail is the similarity detail of one reference sequence
type SimilarityDetail struct {
	// QBegin int
	// QEnd   int
	// TBegin int
	// TEnd   int
	RC bool

	SimilarityScore float64
	Similarity      *SeqComparatorResult
	// Chain           *[]int
	NSeeds int

	// sequence details
	SeqLen int
	SeqID  []byte // seqid of the region
}

func (r *SearchResult) Reset() {
	r.GenomeBatch = -1
	r.GenomeIndex = -1
	r.ID = r.ID[:0]
	r.GenomeSize = 0
	r.Subs = nil
	r.Score = 0
	r.Chains = nil
	r.SimilarityDetails = nil
	r.AlignedFraction = 0
}

// RecycleSearchResults recycles a search result object
func (idx *Index) RecycleSearchResult(r *SearchResult) {
	if r.Subs != nil {
		for _, sub := range *r.Subs {
			poolSub.Put(sub)
		}
		poolSubs.Put(r.Subs)
	}

	if r.Chains != nil {
		for _, chain := range *r.Chains {
			poolChain.Put(chain)
		}
		poolChains.Put(r.Chains)
	}

	// yes, it might be nil for some failed in chaining
	if r.SimilarityDetails != nil {
		idx.RecycleSimilarityDetails(r.SimilarityDetails)
	}

	poolSearchResult.Put(r)
}

// RecycleSimilarityDetails recycles a list of SimilarityDetails
func (idx *Index) RecycleSimilarityDetails(sds *[]*SimilarityDetail) {
	for _, sd := range *sds {
		RecycleSeqComparatorResult(sd.Similarity)
		poolSimilarityDetail.Put(sd)
	}
	poolSimilarityDetails.Put(sds)
}

// RecycleSearchResults recycles search results objects
func (idx *Index) RecycleSearchResults(sr *[]*SearchResult) {
	if sr == nil {
		return
	}

	for _, r := range *sr {
		idx.RecycleSearchResult(r)
	}
	poolSearchResults.Put(sr)
}

var poolSearchResultsMap = &sync.Pool{New: func() interface{} {
	m := make(map[int]*SearchResult, 1024)
	return &m
}}

// --------------------------------------------------------------------------
// searching

// Search queries the index with a sequence.
// After using the result, do not forget to call RecycleSearchResult().
func (idx *Index) Search(s []byte) (*[]*SearchResult, error) {
	// ----------------------------------------------------------------
	// 1) mask the query sequence

	// _kmers, _locses, err := idx.lh.Mask(s, nil)
	_kmers, _locses, err := idx.lh.MaskKnownPrefixes(s, nil)
	if err != nil {
		return nil, err
	}
	defer idx.lh.RecycleMaskResult(_kmers, _locses)

	// ----------------------------------------------------------------
	// 2) matching the captured k-mers in databases

	// a map for collecting matches for each reference: IdIdx -> result
	m := poolSearchResultsMap.Get().(*map[int]*SearchResult)
	clear(*m) // requires go >= v1.21

	inMemorySearch := idx.opt.InMemorySearch

	var searchers []*kv.Searcher
	var searchersIM []*kv.InMemorySearcher
	var nSearchers int

	if inMemorySearch {
		searchersIM = idx.InMemorySearchers
		nSearchers = len(searchersIM)
	} else {
		searchers = idx.Searchers
		nSearchers = len(searchers)
	}

	minPrefix := idx.opt.MinPrefix
	maxMismatch := idx.opt.MaxMismatch

	ch := make(chan *[]*kv.SearchResult, nSearchers)
	done := make(chan int) // later, we will reuse this

	// 2.2) collect search results, they will be kept in RAM.
	// For quries with a lot of hits, the memory would be high.
	// And it's inevitable currently, but if we do want to decrease the memory usage,
	// we can write these matches in temporal files.
	go func() {
		var refpos uint64

		// query substring
		var posQ int
		var beginQ int
		var rcQ bool

		// var code uint64
		var kPrefix int
		var refBatchAndIdx, posT, beginT int
		var mismatch uint8
		var rcT bool

		K := idx.k
		// K8 := idx.k8
		var locs []int
		var sr *kv.SearchResult
		var ok bool

		for srs := range ch {
			// different k-mers in subjects,
			// most of cases, there are more than one
			for _, sr = range *srs {
				// matched length
				kPrefix = int(sr.LenPrefix)
				mismatch = sr.Mismatch

				// locations in the query
				// multiple locations for each QUERY k-mer,
				// but most of cases, there's only one.
				locs = (*_locses)[sr.IQuery] // the mask is unknown
				for _, posQ = range locs {
					rcQ = posQ&1 > 0 // if on the reverse complement sequence
					posQ >>= 1

					// query location
					if rcQ { // on the negative strand
						beginQ = posQ + K - kPrefix
					} else {
						beginQ = posQ
					}

					// matched
					// code = util.KmerPrefix(sr.Kmer, K8, sr.LenPrefix)

					// multiple locations for each MATCHED k-mer
					// but most of cases, there's only one.
					for _, refpos = range sr.Values {
						refBatchAndIdx = int(refpos >> 30) // batch+refIdx
						posT = int(refpos << 34 >> 35)
						rcT = refpos&1 > 0

						// subject location
						if rcT {
							beginT = posT + K - kPrefix
						} else {
							beginT = posT
						}

						_sub2 := poolSub.Get().(*SubstrPair)
						_sub2.QBegin = int32(beginQ)
						_sub2.TBegin = int32(beginT)
						// _sub2.Code = code
						_sub2.Len = uint8(kPrefix)
						_sub2.Mismatch = mismatch
						_sub2.QRC = rcQ
						_sub2.TRC = rcT

						var r *SearchResult
						if r, ok = (*m)[refBatchAndIdx]; !ok {
							subs := poolSubs.Get().(*[]*SubstrPair)
							*subs = (*subs)[:0]

							r = poolSearchResult.Get().(*SearchResult)
							r.GenomeBatch = refBatchAndIdx >> 17
							r.GenomeIndex = refBatchAndIdx & 131071
							r.ID = r.ID[:0] // extract it from genome file later
							r.GenomeSize = 0
							r.Subs = subs
							r.Score = 0
							r.Chains = nil            // important
							r.SimilarityDetails = nil // important
							r.AlignedFraction = 0

							(*m)[refBatchAndIdx] = r
						}

						*r.Subs = append(*r.Subs, _sub2)
					}
				}
			}

			kv.RecycleSearchResults(srs)
		}
		done <- 1
	}()

	// 2.1) search with multiple searchers

	var wg sync.WaitGroup
	var beginM, endM int // range of mask of a chunk
	for iS := 0; iS < nSearchers; iS++ {
		if inMemorySearch {
			beginM = searchersIM[iS].ChunkIndex
			endM = searchersIM[iS].ChunkIndex + searchersIM[iS].ChunkSize
		} else {
			beginM = searchers[iS].ChunkIndex
			endM = searchers[iS].ChunkIndex + searchers[iS].ChunkSize
		}

		wg.Add(1)
		go func(iS, beginM, endM int) {
			idx.searcherTokens[iS] <- 1 // get the access to the searcher
			var srs *[]*kv.SearchResult
			var err error
			if inMemorySearch {
				srs, err = searchersIM[iS].Search((*_kmers)[beginM:endM], minPrefix, maxMismatch)
			} else {
				srs, err = searchers[iS].Search((*_kmers)[beginM:endM], minPrefix, maxMismatch)
			}
			if err != nil {
				checkError(err)
			}

			if len(*srs) == 0 { // no matcheds
				kv.RecycleSearchResults(srs)
			} else {
				ch <- srs // send result
			}

			<-idx.searcherTokens[iS] // return the access
			wg.Done()
		}(iS, beginM, endM)
	}
	wg.Wait()
	close(ch)
	<-done

	if len(*m) == 0 { // no results
		poolSearchResultsMap.Put(m)
		return nil, nil
	}

	// ----------------------------------------------------------------
	// 3) chaining matches for all reference genomes, and alignment

	minSinglePrefix := idx.opt.MinSinglePrefix

	// 3.1) preprocess substring matches for each reference genome
	rs := poolSearchResults.Get().(*[]*SearchResult)
	*rs = (*rs)[:0]

	K := idx.k
	checkMismatch := maxMismatch >= 0 && maxMismatch < K-int(idx.opt.MinPrefix)
	for _, r := range *m {
		ClearSubstrPairs(r.Subs, K) // remove duplicates and nested anchors

		// there's no need to chain for a single short seed.
		// we might give it a chance if the mismatch is low
		if len(*r.Subs) == 1 {
			if checkMismatch {
				if int((*r.Subs)[0].Mismatch) > maxMismatch {
					idx.RecycleSearchResult(r)
					continue
				}
			} else if (*r.Subs)[0].Len < minSinglePrefix {
				// do not forget to recycle filtered result
				idx.RecycleSearchResult(r)
				continue
			}
		}

		for _, sub := range *r.Subs {
			r.Score += float64(sub.Len * sub.Len)
		}

		*rs = append(*rs, r)
	}

	// sort subjects in descending order based on the score (simple statistics).
	// just use the standard library for a few seed pairs.
	sort.Slice(*rs, func(i, j int) bool {
		return (*rs)[i].Score > (*rs)[j].Score
	})

	poolSearchResultsMap.Put(m)

	// 3.2) only keep the top N targets
	topN := idx.opt.TopN
	if topN > 0 && len(*rs) > topN {
		var r *SearchResult
		for i := topN; i < len(*rs); i++ {
			r = (*rs)[i]

			// do not forget to recycle the filtered result
			idx.RecycleSearchResult(r)
		}
		*rs = (*rs)[:topN]
	}

	// 3.3) chaining and alignment

	rs2 := poolSearchResults.Get().(*[]*SearchResult)
	*rs2 = (*rs2)[:0]

	ch2 := make(chan *SearchResult, idx.opt.NumCPUs)
	tokens := make(chan int, idx.opt.NumCPUs)

	// collect hits with good alignment
	go func() {
		for r := range ch2 {
			*rs2 = append(*rs2, r)
		}

		done <- 1
	}()

	cpr := idx.poolSeqComparator.Get().(*SeqComparator)
	// recycle the previou tree data
	cpr.RecycleIndex()
	err = cpr.Index(s) // index the query sequence
	if err != nil {
		checkError(err)
	}

	for _, r := range *rs {
		tokens <- 1
		wg.Add(1)

		go func(r *SearchResult) {
			minChainingScore := idx.chainingOptions.MinScore
			minAF := idx.opt.MinQueryAlignedFractionInAGenome
			extLen := idx.opt.ExtendLength
			contigInterval := idx.contigInterval
			outSeq := idx.opt.OutputSeq

			// -----------------------------------------------------
			// chaining
			chainer := idx.poolChainers.Get().(*Chainer)
			r.Chains, r.Score = chainer.Chain(r.Subs)

			defer func() {
				idx.poolChainers.Put(chainer)
				<-tokens
				wg.Done()
			}()

			if r.Score < minChainingScore {
				idx.RecycleSearchResult(r) // do not forget to recycle unused objects
				return
			}

			// -----------------------------------------------------
			// alignment

			refBatch := r.GenomeBatch
			refID := r.GenomeIndex

			var rdr *genome.Reader
			// sequence reader
			if idx.hasGenomeRdrs {
				rdr = <-idx.poolGenomeRdrs[refBatch]
			} else {
				idx.openFileTokens <- 1 // genome file
				fileGenome := filepath.Join(idx.path, DirGenomes, batchDir(refBatch), FileGenomes)
				rdr, err = genome.NewReader(fileGenome)
				if err != nil {
					checkError(fmt.Errorf("failed to read genome data file: %s", err))
				}
			}

			var sub *SubstrPair
			qlen := len(s)
			var rc bool
			var qb, qe, tb, te, tBegin, tEnd, qBegin, qEnd int
			var l, iSeq, iSeqPre, tPosOffsetBegin, tPosOffsetEnd int
			var tPosOffsetBeginPre int
			var _begin, _end int

			sds := poolSimilarityDetails.Get().(*[]*SimilarityDetail) // HSPs in a reference
			*sds = (*sds)[:0]

			// fragments of a HSP.
			// Since HSP fragments in a HSP might comefrom different contigs.
			// Multiple contigs are concatenated, remember?
			// So we need to create seperate HPSs for these fragments.
			var crChains2 *[]*Chain2Result

			// for remove duplicated alignments
			var duplicated bool
			bounds := poolBounds.Get().(*[]int)
			*bounds = (*bounds)[:0]
			var bi, bend int

			// check sequences from all chains
			for _, chain := range *r.Chains { // for each HSP
				// ------------------------------------------------------------------------
				// extract subsequence from the refseq for comparing

				// fmt.Printf("----------------- [ chain %d ] --------------\n", i)
				// for _i, _c := range *chain {
				// 	fmt.Printf("  %d, %s\n", _i, (*r.Subs)[_c])
				// }

				// the first seed pair
				sub = (*r.Subs)[(*chain)[0]]
				// fmt.Printf("  first: %s\n", sub)
				qb = int(sub.QBegin)
				tb = int(sub.TBegin)

				// the last seed pair
				sub = (*r.Subs)[(*chain)[len(*chain)-1]]
				// fmt.Printf("  last: %s\n", sub)
				qe = int(sub.QBegin) + int(sub.Len) - 1
				te = int(sub.TBegin) + int(sub.Len) - 1
				// fmt.Printf("  (%d, %d) vs (%d, %d) rc:%v\n", qb, qe, tb, te, rc)

				if len(*chain) == 1 { // if there's only one seed, need to check the strand information
					rc = sub.QRC != sub.TRC
				} else { // check the strand according to coordinates of seeds
					rc = tb > int(sub.TBegin)
				}
				// fmt.Printf("  rc: %v\n", rc)

				// extend the locations in the reference
				if rc { // reverse complement
					// tBegin = int(sub.TBegin) - min(qlen-qe-1, extLen)
					tBegin = int(sub.TBegin) - extLen
					if tBegin < 0 {
						tBegin = 0
					}
					// tEnd = tb + int(sub.Len) - 1 + min(qb, extLen)
					tEnd = tb + int(sub.Len) - 1 + extLen
				} else {
					// tBegin = tb - min(qb, extLen)
					tBegin = tb - extLen
					if tBegin < 0 {
						tBegin = 0
					}
					// tEnd = te + min(qlen-qe-1, extLen)
					tEnd = te + extLen
				}

				// extend the locations in the query
				qBegin = qb - min(qb, extLen)
				qEnd = qe + min(qlen-qe-1, extLen)

				// fmt.Printf("---------\nchain:%d, query:%d-%d, subject:%d.%d:%d-%d, rc:%v\n", i+1, qBegin+1, qEnd+1, refBatch, refID, tBegin+1, tEnd+1, rc)

				// extract target sequence for comparison.
				// Right now, we fetch seq from disk for each seq,
				// In the future, we might buffer frequently accessed references for improving speed.
				tSeq, err := rdr.SubSeq(refID, tBegin, tEnd)
				if err != nil {
					checkError(err)
				}
				// this happens when the matched sequene is the last one in the gneome
				if len(tSeq.Seq) < tEnd-tBegin+1 {
					tEnd -= tEnd - tBegin + 1 - len(tSeq.Seq)
				}

				if rc { // reverse complement
					RC(tSeq.Seq)
				}

				// ------------------------------------------------------------------------
				// comparing the two sequences

				cr, err := cpr.Compare(uint32(qBegin), uint32(qEnd), tSeq.Seq, qlen)
				if err != nil {
					checkError(err)
				}
				if cr == nil {
					// recycle target sequence
					genome.RecycleGenome(tSeq)
					continue
				}

				if len(r.ID) == 0 { // record genome information, do it once
					r.ID = append(r.ID, tSeq.ID...)
					r.GenomeSize = tSeq.GenomeSize
				}

				iSeqPre = -1 // the index of previous sequence in this HSP
				tPosOffsetBeginPre = -1

				crChains2 = poolChains2.Get().(*[]*Chain2Result)
				*crChains2 = (*crChains2)[:0]

				for _, c := range *cr.Chains { // for each HSP fragment
					qb, qe, tb, te = c.QBegin, c.QEnd, c.TBegin, c.TEnd
					// fmt.Printf("q: %d-%d, t: %d-%d\n", qb, qe, tb, te)
					// fmt.Printf("--- HSP: %d, HSP fragment: %d ---\n", i, _i)

					// ------------------------------------------------------------
					// get the index of target seq according to the position

					iSeq = 0
					tPosOffsetBegin = 0 // start position of current sequence
					tPosOffsetEnd = 0   // end pososition of current sequence
					var j int
					if tSeq.NumSeqs > 1 { // just for genomes with multiple contigs
						iSeq = -1
						// ===========aaaaaaa================================aaaaaaa=======
						//                   | tPosOffsetBegin              | tPosOffsetEnd
						//                     tb ---------------te (matched region, substring region)

						// fmt.Printf("genome: %s, nSeqs: %d\n", tSeq.ID, tSeq.NumSeqs)
						// fmt.Printf("tBegin: %d, tEnd: %d, tb: %d, te: %d, rc: %v\n", tBegin, tEnd, tb, te, rc)

						// minusing K is because the interval A's might be matched.
						if rc {
							_begin, _end = tEnd-te+K, tEnd-tb-K
						} else {
							_begin, _end = tBegin+tb+K, tBegin+te-K
						}

						// fmt.Printf("  try %d: %d-%d\n", j, _begin, _end)

						for j, l = range tSeq.SeqSizes {
							// end position of current contig
							tPosOffsetEnd += l - 1 // length sum of 0..j

							// fmt.Printf("  seq %d: %d-%d\n", j, tPosOffsetBegin, tPosOffsetEnd)

							if _begin >= tPosOffsetBegin && _end <= tPosOffsetEnd {
								iSeq = j
								// fmt.Printf("iSeq: %d, tPosOffsetBegin: %d, tPosOffsetEnd: %d, seqlen: %d\n",
								// 	iSeq, tPosOffsetBegin, tPosOffsetEnd, l)
								break
							}

							tPosOffsetEnd += contigInterval + 1
							tPosOffsetBegin = tPosOffsetEnd // begin position of the next contig
						}

						// it will not happen now.
						if iSeq < 0 { // this means the aligned sequence crosses two sequences.
							// fmt.Printf("invalid fragment: seqid: %s, aligned: %d, %d-%d, rc:%v, %d-%d\n",
							// 	tSeq.ID, cr.AlignedBases, tBegin, tEnd, rc, _begin, _end)

							poolChain2.Put(c)

							continue
						}

						if iSeqPre >= 0 && iSeq != iSeqPre { // two HSP fragments belong to different sequences ~~~~~
							// fmt.Printf("  %d != %d\n", iSeq, iSeqPre)

							// ------------------------------------------------------------
							// convert the positions

							// fmt.Printf("  aligned: (%d, %d) vs (%d, %d) rc:%v\n", qb, qe, tb, te, rc)
							c.QBegin = qb
							c.QEnd = qe
							if rc {
								c.TBegin = tBegin - tPosOffsetBegin + (len(tSeq.Seq) - te - 1)
								if c.TBegin < 0 { // position in the interval
									c.QEnd += c.TBegin
									c.AlignedBasesQ += c.TBegin
									c.TBegin = 0
								}
								c.TEnd = tBegin - tPosOffsetBegin + (len(tSeq.Seq) - tb - 1)
								if c.TEnd > tSeq.SeqSizes[iSeq]-1 {
									c.QBegin += c.TEnd - (tSeq.SeqSizes[iSeq] - 1)
									c.TEnd = tSeq.SeqSizes[iSeq] - 1
								}
							} else {
								// fmt.Printf("tBegin: %d, tPosOffsetBegin: %d, tPosOffsetEnd: %d\n",
								// 	tBegin, tPosOffsetBegin, tPosOffsetEnd)
								// fmt.Printf("tb: %d, te: %d, tBegin+tb: %d, tBegin+te: %d\n", tb, te, tBegin+tb, tBegin+te)
								c.TBegin = tBegin - tPosOffsetBegin + tb
								if c.TBegin < 0 { // position in the interval
									c.QBegin -= c.TBegin
									c.AlignedBasesQ += c.TBegin
									c.TBegin = 0
								}
								c.TEnd = tBegin - tPosOffsetBegin + te
								// fmt.Printf("tmp: t: %d-%d, seqlen: %d \n", c.TBegin, c.TEnd, tSeq.SeqSizes[iSeq])
								if c.TEnd > tSeq.SeqSizes[iSeq]-1 {
									c.QEnd -= c.TEnd - (tSeq.SeqSizes[iSeq] - 1)
									c.TEnd = tSeq.SeqSizes[iSeq] - 1
								}
							}
							// fmt.Printf("  adjusted: (%d, %d) vs (%d, %d) rc:%v\n", c.QBegin, c.QEnd, c.TBegin, c.TEnd, rc)

							// ------------------------------------------------------------

							// fmt.Printf("  add previous one: %d fragments, aligned-bases: %d\n", len(*crChains2), (*crChains2)[0].AlignedBases)

							if len(*crChains2) > 0 { // it might be empty after duplicated results are removed
								// only include valid chains
								r2 := poolSeqComparatorResult.Get().(*SeqComparatorResult)
								r2.Update(crChains2, cr.QueryLen)
								if outSeq {
									if r2.TSeq == nil {
										r2.TSeq = make([]byte, 0, 1024)
									} else {
										r2.TSeq = r2.TSeq[:0]
									}

									if rc {
										r2.TSeq = append(r2.TSeq, tSeq.Seq[tEnd-r2.TEnd-tPosOffsetBeginPre:tEnd-r2.TBegin-tPosOffsetBeginPre+1]...)
									} else {
										r2.TSeq = append(r2.TSeq, tSeq.Seq[tPosOffsetBeginPre+r2.TBegin-tBegin:tPosOffsetBeginPre+r2.TEnd-tBegin+1]...)
									}
								}

								sd := poolSimilarityDetail.Get().(*SimilarityDetail)
								sd.RC = rc
								// sd.Chain = (*r.Chains)[i]
								sd.NSeeds = len(*chain)
								sd.Similarity = r2
								sd.SimilarityScore = float64(r2.AlignedBases) * (*r2.Chains)[0].Pident
								sd.SeqID = sd.SeqID[:0]
								sd.SeqID = append(sd.SeqID, (*tSeq.SeqIDs[iSeq])...)
								sd.SeqLen = tSeq.SeqSizes[iSeq]

								*sds = append(*sds, sd)
							}

							// ----------

							// create anther HSP
							iSeqPre = -1
							tPosOffsetBeginPre = -1
							crChains2 = poolChains2.Get().(*[]*Chain2Result)
							*crChains2 = (*crChains2)[:0]

							*crChains2 = append(*crChains2, c)

							continue
						}
					}
					iSeqPre = iSeq
					tPosOffsetBeginPre = tPosOffsetBegin

					// ------------------------------------------------------------
					// convert the positions

					// fmt.Printf("  aligned: (%d, %d) vs (%d, %d) rc:%v\n", qb, qe, tb, te, rc)
					c.QBegin = qb
					c.QEnd = qe
					if rc {
						c.TBegin = tBegin - tPosOffsetBegin + (len(tSeq.Seq) - te - 1)
						if c.TBegin < 0 { // position in the interval
							c.QEnd += c.TBegin
							c.AlignedBasesQ += c.TBegin
							c.TBegin = 0
						}
						c.TEnd = tBegin - tPosOffsetBegin + (len(tSeq.Seq) - tb - 1)
						if c.TEnd > tSeq.SeqSizes[iSeq]-1 {
							c.QBegin += c.TEnd - (tSeq.SeqSizes[iSeq] - 1)
							c.TEnd = tSeq.SeqSizes[iSeq] - 1
						}
					} else {
						c.TBegin = tBegin - tPosOffsetBegin + tb
						if c.TBegin < 0 { // position in the interval
							c.QBegin -= c.TBegin
							c.AlignedBasesQ += c.TBegin
							c.TBegin = 0
						}
						c.TEnd = tBegin - tPosOffsetBegin + te
						if c.TEnd > tSeq.SeqSizes[iSeq]-1 {
							c.QEnd -= c.TEnd - (tSeq.SeqSizes[iSeq] - 1)
							c.TEnd = tSeq.SeqSizes[iSeq] - 1
						}
					}
					// fmt.Printf("  adjusted: (%d, %d) vs (%d, %d) rc:%v\n", c.QBegin, c.QEnd, c.TBegin, c.TEnd, rc)

					// ------------------------------------------------------------
					// remove duplicated alignments

					bend = len(*bounds) - 4
					duplicated = false
					for bi = 0; bi <= bend; bi += 4 {
						if (*bounds)[bi] == c.QBegin && (*bounds)[bi+1] == c.QEnd &&
							(*bounds)[bi+2] == c.TBegin && (*bounds)[bi+3] == c.TEnd {
							duplicated = true
							break
						}
					}

					if duplicated {
						poolChain2.Put(c)
					} else {
						*crChains2 = append(*crChains2, c)
						*bounds = append(*bounds, c.QBegin)
						*bounds = append(*bounds, c.QEnd)
						*bounds = append(*bounds, c.TBegin)
						*bounds = append(*bounds, c.TEnd)
					}
				}

				// fmt.Printf("  add current one: %d fragments, aligned-bases: %d\n", len(*crChains2), (*crChains2)[0].AlignedBases)

				if iSeq >= 0 {
					if len(*crChains2) > 0 { // it might be empty after duplicated results are removed

						// only include valid chains
						r2 := poolSeqComparatorResult.Get().(*SeqComparatorResult)
						r2.Update(crChains2, cr.QueryLen)
						if outSeq {
							if r2.TSeq == nil {
								r2.TSeq = make([]byte, 0, 1024)
							} else {
								r2.TSeq = r2.TSeq[:0]
							}

							if rc {
								r2.TSeq = append(r2.TSeq, tSeq.Seq[tEnd-r2.TEnd-tPosOffsetBegin:tEnd-r2.TBegin-tPosOffsetBegin+1]...)
							} else {
								r2.TSeq = append(r2.TSeq, tSeq.Seq[tPosOffsetBegin+r2.TBegin-tBegin:tPosOffsetBegin+r2.TEnd-tBegin+1]...)
							}
						}

						sd := poolSimilarityDetail.Get().(*SimilarityDetail)
						sd.RC = rc
						sd.NSeeds = len(*chain)
						sd.Similarity = r2
						sd.SimilarityScore = float64(r2.AlignedBases) * (*r2.Chains)[0].Pident
						sd.SeqID = sd.SeqID[:0]
						sd.SeqID = append(sd.SeqID, (*tSeq.SeqIDs[iSeq])...)
						sd.SeqLen = tSeq.SeqSizes[iSeq]

						*sds = append(*sds, sd)
					}
				}

				// recycle target sequence

				poolChains2.Put(cr.Chains)
				genome.RecycleGenome(tSeq)
			}

			if len(*sds) == 0 { // no valid alignments
				poolSimilarityDetails.Put(sds)
				idx.RecycleSearchResult(r) // do not forget to recycle unused objects

				if idx.hasGenomeRdrs {
					idx.poolGenomeRdrs[refBatch] <- rdr
				} else {
					err = rdr.Close()
					if err != nil {
						checkError(fmt.Errorf("failed to close genome data file: %s", err))
					}
					<-idx.openFileTokens
				}

				return
			}

			// compute aligned bases
			var alignedBasesGenome int
			regions := poolRegions.Get().(*[]*[2]int)
			*regions = (*regions)[:0]
			for _, sd := range *sds {
				for _, c := range *sd.Similarity.Chains {
					region := poolRegion.Get().(*[2]int)
					region[0], region[1] = c.QBegin, c.QEnd
					*regions = append(*regions, region)
				}
			}
			alignedBasesGenome = coverageLen(regions)
			recycleRegions(regions)

			// filter by query coverage per genome
			r.AlignedFraction = float64(alignedBasesGenome) / float64(len(s)) * 100
			if r.AlignedFraction > 100 {
				r.AlignedFraction = 100
			}
			if r.AlignedFraction < minAF { // no valid alignments
				idx.RecycleSimilarityDetails(sds)
				idx.RecycleSearchResult(r) // do not forget to recycle unused objects

				if idx.hasGenomeRdrs {
					idx.poolGenomeRdrs[refBatch] <- rdr
				} else {
					err = rdr.Close()
					if err != nil {
						checkError(fmt.Errorf("failed to close genome data file: %s", err))
					}
					<-idx.openFileTokens
				}
				return
			}

			// r.AlignResults = ars
			sort.Slice(*sds, func(i, j int) bool {
				return (*sds)[i].SimilarityScore > (*sds)[j].SimilarityScore
			})
			r.SimilarityDetails = sds

			// ----------------------------------

			// recycle genome reader
			if idx.hasGenomeRdrs {
				idx.poolGenomeRdrs[refBatch] <- rdr
			} else {
				err = rdr.Close()
				if err != nil {
					checkError(fmt.Errorf("failed to close genome data file: %s", err))
				}
				<-idx.openFileTokens
			}

			// we don't need these data for outputing results.
			// If we do not do this, they will be in memory until the result is outputted.
			// recycle the chain data
			for _, sub := range *r.Subs {
				poolSub.Put(sub)
			}
			poolSubs.Put(r.Subs)
			r.Subs = nil

			if r.Chains != nil {
				for _, chain := range *r.Chains {
					poolChain.Put(chain)
				}
				poolChains.Put(r.Chains)
				r.Chains = nil
			}

			ch2 <- r
		}(r)
	}

	wg.Wait()
	close(ch2)
	<-done
	poolSearchResults.Put(rs)

	// recycle this comparator
	idx.poolSeqComparator.Put(cpr)

	// sort all hits
	if len(*rs2) == 0 {
		poolSearchResults.Put(rs2)
		return nil, nil
	}

	sort.Slice(*rs2, func(i, j int) bool {
		return (*(*rs2)[i].SimilarityDetails)[0].SimilarityScore > (*(*rs2)[j].SimilarityDetails)[0].SimilarityScore
	})

	return rs2, nil
}

// RC computes the reverse complement sequence
func RC(s []byte) []byte {
	n := len(s)
	for i := 0; i < n; i++ {
		s[i] = rcTable[s[i]]
	}
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
	return s
}

var rcTable = [256]byte{
	0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15,
	16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31,
	32, 33, 34, 35, 36, 37, 38, 39, 40, 41, 42, 43, 44, 45, 46, 47,
	48, 49, 50, 51, 52, 53, 54, 55, 56, 57, 58, 59, 60, 61, 62, 63,
	64, 84, 86, 71, 72, 69, 70, 67, 68, 73, 74, 77, 76, 75, 78, 79,
	80, 81, 89, 83, 65, 85, 66, 87, 88, 82, 90, 91, 92, 93, 94, 95,
	96, 116, 118, 103, 104, 101, 102, 99, 100, 105, 106, 109, 108, 107, 110, 111,
	112, 113, 121, 115, 97, 117, 98, 119, 120, 114, 122, 123, 124, 125, 126, 127,
	128, 129, 130, 131, 132, 133, 134, 135, 136, 137, 138, 139, 140, 141, 142, 143,
	144, 145, 146, 147, 148, 149, 150, 151, 152, 153, 154, 155, 156, 157, 158, 159,
	160, 161, 162, 163, 164, 165, 166, 167, 168, 169, 170, 171, 172, 173, 174, 175,
	176, 177, 178, 179, 180, 181, 182, 183, 184, 185, 186, 187, 188, 189, 190, 191,
	192, 193, 194, 195, 196, 197, 198, 199, 200, 201, 202, 203, 204, 205, 206, 207,
	208, 209, 210, 211, 212, 213, 214, 215, 216, 217, 218, 219, 220, 221, 222, 223,
	224, 225, 226, 227, 228, 229, 230, 231, 232, 233, 234, 235, 236, 237, 238, 239,
	240, 241, 242, 243, 244, 245, 246, 247, 248, 249, 250, 251, 252, 253, 254, 255,
}

var poolBounds = &sync.Pool{New: func() interface{} {
	tmp := make([]int, 128)
	return &tmp
}}

func parseKmerValue(v uint64) (int, int, int, int) {
	return int(v >> 47), int(v << 17 >> 47), int(v << 34 >> 35), int(v & 1)
}

func kmerValueString(v uint64) string {
	return fmt.Sprintf("batchIdx: %d, genomeIdx: %d, pos: %d, rc: %v",
		int(v>>47), int(v<<17>>47), int(v<<34>>35), v&1 > 0)
}
