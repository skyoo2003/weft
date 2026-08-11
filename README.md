# weft

> 씨실. 날실(신호)들은 서로 닿지 않고, 씨실 하나가 전부를 가로질러 묶는다.

**신호 무관 융합 검색 엔진.** Go, 밑바닥부터, 표준 라이브러리만.

```go
// 모든 신호가 이것만 구현한다. 융합은 이것만 안다.
type Scorer interface {
    Name() string
    Candidates(ctx context.Context, q Query, k int) ([]Candidate, error)
}

// 신호가 몇 개인지도, 어떤 종류인지도 모른다.
func Fuse(streams [][]Candidate, k int) []Candidate
```

> 문서는 이 개념을 **신호**라고 부르고 코드에서는 `Scorer`, 패키지로는 `pkg/scorer/`다. 같은 것이다.

## 왜 또 검색 엔진인가

하이브리드 검색 엔진들은 **하나의 신호로 시작해 나머지를 나중에 덧붙였다.** 역색인 위에 벡터를 붙이고, 그 위에 융합 랭킹을 얹는다. 그래서 융합은 언제나 특수 케이스다 — 두 신호를 합치는 전용 코드 경로가 있고, 세 번째 신호를 넣으려면 그 경로를 다시 뜯어야 한다. 그래프 근접도가 어느 엔진에서도 1급 랭킹 신호가 아닌 이유가 이것이다.

weft는 순서를 뒤집는다: **융합을 기본 동작으로 놓고 신호를 그 위에 꽂는다.** 네 번째 신호를 추가하는 비용이 첫 번째와 같아진다.

**정직하게**: 텍스트+벡터 하이브리드만 필요하다면 [bleve](https://github.com/blevesearch/bleve)를 쓰는 게 맞다. 이미 BM25·ANN·RRF를 내장한다. weft는 시장 공백이 아니라 **아키텍처 가설** 위에 서 있다. 자세한 내용은 [`.claude/prds/weft.prd.md`](.claude/prds/weft.prd.md).

## 상태

**마일스톤 1 통과** — 아키텍처 가설이 섰다. 판정 근거와 남은 비용은 [`docs/FINDINGS.md`](docs/FINDINGS.md).

| # | 마일스톤 | 상태 |
|---|---|---|
| 1 | 신호 무관 융합 | ✅ 통과 (단언 3/3) |
| 2 | 영속화 | 대기 |
| 3 | 규모 (세그먼트 병합, ANN) | 대기 |
| 4 | 품질 증명 (nDCG) | 대기 |
| 5 | 성능 증명 (GC 포함 p99) | 대기 |
| 6 | 외부 채택 가능 상태 | 대기 |

**프로덕션에 쓰지 마세요.** 전부 인메모리이고, 영속화가 없고, 재시작하면 색인이 사라진다.

## 빠른 시작

```bash
go run ./cmd/weft
```

```
weft — 8 documents, 4 scorers. Query syntax: TEXT [@ v1,v2,v3]. Ctrl-D to quit.

query> ranking fusion
  1. rrf        0.03226  text:2  vector:-  graph:-  recency:2
  2. hnsw       0.03200  text:-  vector:-  graph:2  recency:3
  3. bm25       0.03178  text:-  vector:-  graph:1  recency:5
  4. ivf        0.03150  text:-  vector:-  graph:3  recency:4
  5. tfidf      0.01639  text:1  vector:-  graph:-  recency:-

query> ranking fusion @ 0,1,0
  1. rrf        0.04813  text:2  vector:3  graph:-  recency:2
  2. hnsw       0.04813  text:-  vector:2  graph:2  recency:3
  3. ivf        0.04789  text:-  vector:1  graph:3  recency:4
  4. bm25       0.04740  text:-  vector:4  graph:1  recency:5
  5. tfidf      0.03178  text:1  vector:5  graph:-  recency:-
```

오른쪽 열이 각 신호가 **융합 전에** 매긴 순위다. 읽는 법:

- `tfidf`는 텍스트 1위인데 융합 5위 — **나머지 셋 중 아무도 동의하지 않았다.** 한 신호의 확신은 합의를 이기지 못한다
- `hnsw`·`bm25`·`ivf`는 텍스트로 전혀 안 잡혔다. 그래프 순회가 찾아낸 문서들이다
- 첫 쿼리의 `vector:-`는 쿼리에 벡터를 안 줬기 때문. **의견 없는 신호는 결과를 왜곡하지 않고 비용도 0이다.** 두 번째 쿼리에서 `@ 0,1,0`을 붙이자 벡터 신호가 참여하면서 `ivf`가 3위로 올라왔다
- `rrf`·`tfidf`의 `graph:-`는 그 둘이 그래프 순회의 **시드**였다는 뜻이다. 시드는 결과에서 빠진다 — 이유는 [`docs/FINDINGS.md` 3.1절](docs/FINDINGS.md)

라이브러리로 쓰는 최소 예제는 [`examples/basic`](examples/basic/main.go), godoc 예제는 `pkg/engine`의 `Example`.

## 신호 추가하기 — 이 프로젝트의 전부

`engine.Scorer`를 구현하면 끝이다. **`engine/`도 `fusion/`도 건드리지 않는다.**

```go
package popularity

type Scorer struct{ ix *engine.Index }

func New(ix *engine.Index) *Scorer { return &Scorer{ix: ix} }
func (s *Scorer) Name() string     { return "popularity" }

func (s *Scorer) Candidates(ctx context.Context, q engine.Query, k int) ([]engine.Candidate, error) {
    cands := make([]engine.Candidate, 0, s.ix.Len())
    for i := range s.ix.Len() {
        // ... 점수를 매긴다. 스케일은 마음대로 — 융합은 순위만 본다.
    }
    return engine.TopK(cands, k), nil
}
```

호출부에 한 줄 추가:

```go
scorers := []engine.Scorer{txt, vec, gr, rec, popularity.New(ix)}
results, err := engine.Search(ctx, q, 10, fusion.Fuse, scorers...)
```

이게 정말 성립하는지는 `pkg/engine/architecture_test.go`가 기계적으로 검증한다:

```bash
make arch
```

- 융합 불변 — 3신호와 4신호를 같은 호출식으로 호출한다 (컴파일이 증명)
- 추가 비용 — 4번째 신호 구현 71줄(예산 100), `engine/`·`fusion/` 변경 0줄
- 신호 무지 — `go list -deps ./pkg/fusion`에 `scorer/`가 없다

세 번째 단언이 핵심이다. `fusion.Fuse`는 `Candidate.Score`를 **한 번도 읽지 않는다** — 순위만 쓴다. BM25는 무계, 코사인은 `[-1,1]`, 그래프 근접도는 `(0,1]`이라 점수를 비교하려면 정규화가 필요하고, 정규화하려면 어떤 신호인지 알아야 하기 때문이다.

## 구조

```
cmd/weft/          대화형 데모 바이너리
examples/basic/    라이브러리 임베드 최소 예제
pkg/
  engine/          공통 타입, Scorer 인터페이스, 인메모리 색인, Search
  fusion/          RRF — engine만 import한다
  scorer/
    text/          BM25 (ln(1+…) IDF형)
    vector/        브루트포스 코사인
    graph/         시드 BFS, 1/(1+hops)
    recency/       2^(-age/HalfLife) — 아키텍처 검증용 4번째 신호
docs/
  FINDINGS.md      마일스톤 1 판정, 인터페이스 선택의 비용, 마일스톤 2 권고
  DECISIONS.md     되돌리기 비싼 결정만 (D-001: 블록 구조 포스팅 포맷)
  DATASETS.md      마일스톤 4 평가 데이터셋 조사 — 무엇이 실격이고 왜
```

의존 방향은 항상 안쪽이다. `engine`은 weft 패키지를 하나도 import하지 않고, `fusion`은 `engine` 하나만 import한다. `engine.Search`가 `fusion`을 import하지 않고 `Fuser` 함수를 받는 이유도 이것이다 — engine은 신호도 융합 전략도 모른다.

## 개발

```bash
make            # build + vet + test
make arch       # 아키텍처 합격선 (마일스톤 1 판정)
make deps       # 외부 의존성 0개 + 융합이 신호를 모름 확인
make run        # 대화형 데모
make example    # 최소 예제
```

라이브러리 구현 777줄, 테스트 1,426줄, 데모·예제 228줄, 외부 의존성 **0개**. Go 1.26+.

## 알려진 한계

- **영속화 없음** — 전부 인메모리. 마일스톤 2
- **조기 종료 불가** — 상위 k 후보 목록 인터페이스를 골랐고, WAND류 스킵이 구조적으로 안 된다. 대가와 확장 경로는 [`docs/FINDINGS.md` 2절](docs/FINDINGS.md)
- **그래프 시드 선택이 미검증** — 시드 에코로 인한 이중 계산은 시드를 결과에서 제외해 해결했지만([3.1절](docs/FINDINGS.md)), `SeedN = 5`와 "텍스트 상위 n개"라는 출처가 품질에 좋은 선택인지는 마일스톤 4에서만 알 수 있다
- **CJK 토크나이징 없음** — 공백·구두점 분리뿐이라 한글·한자 연속이 한 토큰으로 붙는다
- **임베딩 생성 안 함** — 벡터는 외부에서 주입
- **쿼리 언어 없음** — Go API로만 쿼리를 만든다

## 라이선스

[Apache License 2.0](LICENSE). 서드파티 고지는 [NOTICE](NOTICE) — 요약하면 **없다**, weft는 외부 의존성이 0개다.
