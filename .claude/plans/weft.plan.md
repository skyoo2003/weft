# Plan: weft — Milestone 1

**Source PRD**: `.claude/prds/weft.prd.md` (v3, Go, from scratch)
**Module path**: `github.com/<user>/weft` — 최종 확정은 Task 1에서
**Selected Milestone**: 1 — 신호 무관 융합
**Complexity**: Medium

> **v2와 무엇이 달라졌나**: v2는 bleve 위에 얹는 계획이었다(Complexity: Small). v3는 밑바닥 개발이므로 역색인·BM25·벡터 스캔·그래프 순회를 전부 직접 쓴다. 다만 **인메모리 + 표준 라이브러리만**으로 묶어 수일 규모를 유지한다. 디스크 포맷은 마일스톤 2까지 손대지 않는다 — 잘못된 구조를 디스크에 굳히는 것이 이 프로젝트에서 가장 비싼 실수다.

## Summary
텍스트·벡터·그래프 세 신호가 **동일한 점수 스트림 인터페이스**를 구현하고, 신호의 개수·종류를 모르는 하나의 융합 연산자가 이를 소비해 순위를 내는 인메모리 엔진을 만든다.

이 마일스톤의 합격선은 기능이 아니라 **아키텍처 속성**이다: 세 신호를 꽂은 뒤 **4번째 신호(최신성)를 융합 연산자 코드를 한 줄도 바꾸지 않고 추가**할 수 있어야 한다. 그것이 되면 PRD 가설이 서고, 안 되면 설계를 다시 하거나 접는다.

## Decisions Taken (PRD Open Questions에 대한 기본값)
| PRD Open Question | 이 계획의 기본값 | 이유 |
|---|---|---|
| 점수 스트림 인터페이스 형태 | **상위 k개 후보 목록** (`[]Candidate{DocID, Score}`) — 정렬된 doc id 순회 아님 | 되돌리기 어려운 선택이지만, WAND류 조기 종료는 성능 최적화이고 M1은 아키텍처 검증이다. 조기 종료는 마일스톤 5에서 인터페이스를 확장해 도입. **이 결정을 FINDINGS에 명시해 마일스톤 5가 비용을 알고 시작하게 한다** |
| 융합 방식 | **RRF (순위 기반)** | 신호마다 점수 스케일이 다르다(BM25는 무계, 코사인은 [-1,1], BFS 거리는 정수). RRF는 스케일을 알 필요가 없어 "신호를 모르는 융합" 목표와 정확히 맞는다. 가중 점수합은 정규화가 필요해 융합이 신호를 알게 된다 |
| 그래프 근접도 점수 | **시드로부터의 BFS 거리, 점수 = 1/(1+거리)** | 가장 단순한 진짜 근접도. 시드는 텍스트 1차 결과 상위 n개. PageRank류는 마일스톤 4에서 품질이 부족할 때 |
| 토크나이저 | **소문자화 + 공백/구두점 분리** | 형태소 분석은 PRD Out of scope. 아키텍처와 무관 |
| 영속화 | **없음. 전부 인메모리** | 마일스톤 2의 몫. 여기서 하면 잘못된 구조를 디스크에 굳힌다 |

## Patterns to Mirror
**해당 없음 — 그린필드.** `/Users/lukas/Workspace/weft` 에는 `.claude/` 외 파일이 없고, 커밋도 0개다. 미러링할 패턴을 지어내지 않는다.

M1이 관례를 **정하는** 위치이므로 아래를 기준선으로 삼고 이후 마일스톤이 미러링한다.

| Category | 이 마일스톤이 세우는 기준 |
|---|---|
| Naming | 패키지 = 역할 (`engine`, `signal/text`, `signal/vector`, `signal/graph`, `fusion`). 신호는 전부 `signal/` 아래 — 이 디렉터리 구조 자체가 아키텍처 주장이다 |
| Errors | `fmt.Errorf("...: %w", err)` 래핑, 센티널은 `var ErrXxx = errors.New(...)`. 라이브러리 코드에서 `panic` 금지 |
| Logging | 도입하지 않음 — 테스트가 유일한 관찰 수단 |
| Data access | 색인 쓰기는 단일 진입점, 신호는 읽기 전용으로 색인을 본다. 신호가 자기 저장소를 따로 갖지 않는 것이 검증 대상 |
| Tests | 표준 `testing`, table-driven. 외부 테스트 프레임워크 도입 안 함 |
| 의존성 | **표준 라이브러리만.** `go.mod` 의 `require` 블록이 비어 있어야 한다 |

## 핵심 설계 — 이 마일스톤의 전부
```go
// 모든 신호가 이것만 구현한다. 융합은 이것만 안다.
type Signal interface {
    Name() string
    Candidates(ctx context.Context, q Query, k int) ([]Candidate, error)
}

type Candidate struct {
    Doc   DocID
    Score float64  // 신호 내부 스케일. 융합은 이 값의 의미를 모른다.
}

// 신호 개수도 종류도 모른다. 이 시그니처가 바뀌면 가설 실패.
func Fuse(streams [][]Candidate, k int) []Candidate
```
`Fuse` 는 순위만 쓴다(RRF). 신호가 3개든 10개든 시그니처가 같다. **`Fuse` 안에 `switch signal.Name()` 이 등장하는 순간 이 마일스톤은 실패한 것이다.**

## 사다리 근거 (밑바닥 개발 안에서의 최소화)
"직접 만든다"는 전제는 고정. 그 안에서 무엇을 안 만들지는 여전히 선택이다.

| 필요 | 선택 | 안 만드는 것 |
|---|---|---|
| 텀 딕셔너리 | `map[string][]Posting` | FST·트라이·디스크 포맷 — 마일스톤 2 |
| 포스팅 리스트 | `[]Posting{DocID, TermFreq}` 슬라이스 | 델타·varint 인코딩, 스킵 리스트 — 마일스톤 3 |
| BM25 | 표준 공식 직접 구현 (아래 명시) | 없음. 이건 핵심이고 정확해야 한다 |
| 벡터 kNN | 브루트포스 코사인 + `container/heap` 상위 k | HNSW·IVF — 마일스톤 3 |
| 그래프 순회 | `map[DocID][]DocID` + BFS (`container/list` 또는 슬라이스 큐) | 그래프 라이브러리, 디스크 인접 리스트 |
| 정렬·힙 | `sort`, `container/heap` | 자체 정렬 |
| 융합 | RRF, 수식 한 줄 | 학습 기반 랭커 — 범위 밖 |

**BM25는 아래 형태로 구현한다** (IDF 음수와 0 나눗셈을 피하는 표준형):
```
IDF(q)   = ln(1 + (N - n(q) + 0.5) / (n(q) + 0.5))
score(D) = Σ IDF(q) · f(q,D)·(k1+1) / (f(q,D) + k1·(1 - b + b·|D|/avgdl))
k1 = 1.2, b = 0.75
```
게으르게 짜되 경계에서 틀리지 않는다 — `n(q)=0`, `|D|=0`, `avgdl=0` 을 테스트로 고정한다.

## Files to Change
| File | Action | Why |
|---|---|---|
| `go.mod` | CREATE | 모듈 정의. **`require` 비어 있음** |
| `engine/doc.go` | CREATE | `DocID`, `Document`, `Candidate`, `Query` — 공통 타입 |
| `engine/signal.go` | CREATE | **`Signal` 인터페이스 ★** — 이 파일이 아키텍처 주장 자체 |
| `engine/index.go` | CREATE | 인메모리 색인. 단일 `Add` 진입점, 신호들이 읽는 공유 상태 |
| `signal/text/text.go` | CREATE | 토크나이저, 역색인 조회, BM25 스코어러 |
| `signal/vector/vector.go` | CREATE | 브루트포스 코사인 + 상위 k 힙 |
| `signal/graph/graph.go` | CREATE | 인접 리스트, 시드로부터 BFS, 1/(1+거리) 점수 |
| `signal/recency/recency.go` | CREATE | **4번째 신호 — 아키텍처 검증용.** 문서 타임스탬프 기반. 이게 100줄 미만이고 `fusion` 을 안 건드리면 합격 |
| `fusion/rrf.go` | CREATE | `Fuse([][]Candidate, k)` — 신호를 모르는 RRF |
| `engine/search.go` | CREATE | 신호들을 실행하고 `Fuse` 에 넘기는 오케스트레이션 |
| `*_test.go` | CREATE | 각 패키지 단위 테스트 + `engine/architecture_test.go` (합격선) |
| `example_test.go` | CREATE | Go 예제 테스트 = 문서 + 검증 동시 달성 |
| `FINDINGS.md` | CREATE | 아키텍처 가설 판정, 인터페이스 선택의 비용, 마일스톤 2 권고 |

## Tasks

### Task 1: 공통 타입 + Signal 인터페이스 + 인메모리 색인
- **Action**: `engine/` 에 `DocID`, `Document`(텍스트·벡터·엣지·타임스탬프), `Candidate`, `Query`, `Signal` 인터페이스, 그리고 단일 `Add` 진입점을 가진 인메모리 색인. **신호 구현은 아직 없음** — 인터페이스만 먼저 확정
- **Mirror**: 위 기준선 표의 Naming·Errors 규칙
- **Validate**: `go build ./... && go vet ./...`, `go list -m all` 이 자기 모듈만 출력

### Task 2: 텍스트 신호 (역색인 + BM25) — 가장 큼
- **Action**: 토크나이저, `map[string][]Posting` 역색인, BM25 스코어러. 위 공식 그대로. `Signal` 구현
- **Mirror**: Task 1의 인터페이스, 에러 래핑 규약
- **Validate**: `go test ./signal/text/...` — table-driven. **경계 케이스 필수**: 없는 텀(`n(q)=0`), 빈 문서, 단일 문서 코퍼스(`avgdl` 계산), 동일 텀 반복(길이 정규화가 실제로 작동하는지)

### Task 3: 벡터 신호 (브루트포스 코사인)
- **Action**: 전체 스캔 코사인 유사도 + `container/heap` 상위 k. `Signal` 구현. **`ponytail:` 주석으로 O(n·d) 천장과 승급 경로(HNSW, 마일스톤 3) 명시**
- **Mirror**: Task 2의 `Signal` 구현 형태
- **Validate**: `go test ./signal/vector/...` — 직교 벡터 = 0, 동일 벡터 = 1, 차원 불일치 시 에러, 영벡터에서 0 나눗셈 없음

### Task 4: 그래프 신호 (시드 BFS 근접도)
- **Action**: `map[DocID][]DocID` 인접 리스트. 시드 집합(텍스트 1차 결과 상위 n개)에서 depth 제한 BFS, 점수 `1/(1+거리)`. `Signal` 구현
- **Mirror**: Task 2–3의 `Signal` 구현 형태
- **Validate**: `go test ./signal/graph/...` — 사이클 있는 그래프에서 무한루프 없음, 도달 불가 노드 제외, depth 경계 정확, 삭제된 doc id를 가리키는 dangling 엣지 무시

### Task 5: 융합 연산자 (신호를 모르는 RRF)
- **Action**: `fusion/rrf.go` — `Fuse(streams [][]Candidate, k int) []Candidate`. RRF: `Σ 1/(60 + rank)`. **`switch` 나 신호 이름 분기 금지.** `engine/search.go` 가 신호들을 실행해 `Fuse` 에 넘김
- **Mirror**: 패키지 = 역할 규칙
- **Validate**: `go test ./fusion/...` — 스트림 1개면 원래 순위 보존, 스트림 0개면 빈 결과(패닉 아님), 양쪽 상위에 있는 문서가 한쪽만 상위인 문서보다 위

### Task 6: 아키텍처 검증 ★ — 이 마일스톤의 합격선
- **Action**: `signal/recency/` 에 **4번째 신호**를 추가한다(문서 타임스탬프 기반). 그리고 `engine/architecture_test.go` 에 3개 단언:
  1. **융합 불변** — 신호 3개일 때와 4개일 때 `Fuse` 를 같은 코드로 호출한다 (컴파일이 곧 증명)
  2. **추가 비용** — `signal/recency/` 전체가 100줄 미만이고, `fusion/` 과 `engine/` 의 diff가 **0줄**
  3. **신호 무지** — `fusion/` 패키지가 어떤 `signal/*` 패키지도 import 하지 않는다 (테스트에서 import 그래프 확인, 또는 `go list -deps` 로 검증)
  단언 2·3 중 하나라도 깨지면 **가설 실패** — FINDINGS.md에 원인을 적고 설계를 다시 한다
- **Mirror**: 표준 `testing` 규칙
- **Validate**: `go test ./engine/...` 전부 통과

### Task 7: 예제 + FINDINGS.md
- **Action**: `example_test.go` 로 end-to-end 시연(Go 예제 테스트라 문서이자 검증). `FINDINGS.md` 에 3가지 기록:
  1. **아키텍처 가설 판정** — Task 6 단언 3개의 결과
  2. **인터페이스 선택의 비용** — "상위 k개 후보 목록" 을 고른 대가로 조기 종료(WAND)가 불가능함. 마일스톤 5에서 인터페이스를 어떻게 확장해야 하는지 지금 알고 있는 만큼 기록
  3. **마일스톤 2 권고** — 디스크 포맷으로 굳히기 전에 바꿔야 할 것
- **Mirror**: 해당 없음
- **Validate**: `go test ./...` 전체 통과, 사람이 FINDINGS를 읽고 마일스톤 2 진행 여부를 결정 가능

## Validation
```bash
go version                       # go1.26.1 확인됨
go build ./...                   # 컴파일
go vet ./...                     # 정적 분석
go test ./...                    # 전체
go test ./engine/...             # 아키텍처 합격선
go list -m all                   # 의존성 0 확인 — 자기 모듈만 출력되어야 함
go list -deps ./fusion | grep signal   # 출력 없어야 함 (융합이 신호를 모름)
```

## Risks
| Risk | Likelihood | Mitigation |
|---|---|---|
| **BM25 구현이 미묘하게 틀림** (IDF 음수, avgdl 0 나눗셈, 길이 정규화 방향 반대) | **High** | Task 2의 경계 테스트를 먼저 쓴다. 표준 공식을 계획서에 박아둔 이유가 이것 |
| **"상위 k개 후보" 인터페이스가 성능 한계로 판명** — 조기 종료 불가 | **High** | 이미 결정 사항으로 명시. Task 7이 그 비용을 문서화해 마일스톤 5가 눈뜨고 시작하게 함 |
| 그래프 시드를 텍스트 결과에서 뽑으면 그래프 신호가 텍스트에 종속됨 (독립 신호가 아님) | **High** | Task 4에서 드러난다. 독립성이 필요하면 시드를 쿼리 파라미터로 받는 경로를 함께 제공. FINDINGS에 기록 |
| `Fuse` 에 신호별 분기가 슬며시 들어옴 | Medium | Task 6 단언 3(import 그래프 검사)이 기계적으로 잡는다 — 사람 리뷰에 의존하지 않음 |
| 인메모리 전제가 마일스톤 2에서 전면 재작성을 부름 | Medium | 의도된 비용. 잘못된 구조를 디스크에 굳히는 것보다 싸다 |
| 범위가 예상보다 커져 수일이 수주가 됨 | Medium | Task 2가 가장 크다. 3일 넘어가면 토크나이저·스코어러를 더 단순화하고 FINDINGS에 기록 |

## Acceptance
- [ ] Task 1–7 완료
- [ ] `go build ./...`, `go vet ./...`, `go test ./...` 전부 통과
- [ ] `go list -m all` 이 자기 모듈만 출력 (외부 의존성 0)
- [ ] `go list -deps ./fusion` 에 `signal/` 이 없음 — **융합이 신호를 모름**
- [ ] 4번째 신호 추가 시 `fusion/`·`engine/` diff 0줄, 신규 코드 100줄 미만
- [ ] BM25 경계 케이스 4종(없는 텀, 빈 문서, 단일 문서, 텀 반복) 테스트 통과
- [ ] `FINDINGS.md` 에 아키텍처 판정·인터페이스 비용·마일스톤 2 권고 기록
- [ ] 지어낸 패턴 없음 (그린필드이므로 미러링 대상 없음을 명시함)

## Estimated Effort
- Task 1 (타입 + 인터페이스): 2–3시간 — **가장 중요하므로 서두르지 말 것**
- Task 2 (텍스트 + BM25): **1–2일** — 이 마일스톤에서 가장 큼
- Task 3 (벡터): 2–3시간
- Task 4 (그래프): 3–4시간
- Task 5 (융합): 2–3시간
- Task 6 (아키텍처 검증): 3–4시간
- Task 7 (예제 + FINDINGS): 2–3시간
- 합계: **3–5일**

---
*Status: AWAITING CONFIRMATION — 코드 작성 전 승인 필요.*
