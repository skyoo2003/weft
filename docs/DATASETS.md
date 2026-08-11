# 그래프 신호 평가용 데이터셋 조사

**질문** (PRD Open Question, [FINDINGS 5절 권고 5](FINDINGS.md)): 그래프 근접도가 nDCG를 올리는지 측정할 수 있는, 관련성 라벨 있는 공개 데이터셋이 존재하는가? 없으면 마일스톤 4가 성립하지 않는다.

**답: 존재한다. 마일스톤 4는 성립한다.** 다만 곧바로 쓸 수 있는 단일 데이터셋은 없고, **사람이 만든 라벨 + 외부에서 조인한 링크 그래프** 조합이어야 한다. 이유는 아래 실격 기준에 있다.

**마일스톤 2 범위에 미치는 영향: 없음.** 그래프를 1급으로 영속화하는 계획을 바꿀 이유가 없다. (이 조사를 M2보다 먼저 한 목적이 그 확인이었다.)

---

## 실격 기준 — 라벨이 링크에서 나온 데이터셋은 못 쓴다

weft의 그래프 스코어러는 시드로부터의 홉 거리를 `1/(1+hops)`로 환산한다. **정답 라벨이 같은 링크 구조에서 파생된 데이터셋을 쓰면 그래프 신호가 라벨 생성 규칙을 재현하는 것이므로 nDCG가 오르는 게 당연하다.** 측정이 아니라 순환 논증이다.

[FINDINGS 3.1](FINDINGS.md)의 시드 에코와 같은 실패 유형이다. 그때는 텍스트 한 표를 두 번 셌고, 이번엔 정답지를 보고 시험을 치는 셈이다.

---

## 실격 판정

### ❌ NFCorpus — 가장 위험한 near-miss

BEIR에 들어 있고 3,633 문서 / 323 쿼리로 **크기가 완벽하다.** 인메모리 엔진에 딱 맞아서 제일 먼저 손이 가는 후보다.

그런데 라벨 생성 방식이 이렇다:

| 등급 | 생성 규칙 |
|---|---|
| 최상위 | NutritionFacts 기사(쿼리)가 의학 문서를 **직접 링크** |
| 차상위 | 쿼리가 다른 NutritionFacts 기사를 링크하고 그 기사가 문서를 링크 (**2홉**) |
| 최하위 | 태그·토픽 시스템으로 연결 |

**이건 `1/(1+hops)`와 사실상 같은 함수다.** 원본 169,756개 판정이 전부 "automatically extracted" — 사람이 본 게 아니다. 이걸로 측정하면 그래프 신호가 압도적으로 이기고, 그 수치는 아무 의미가 없다.

크기가 매력적이라 나중에 누가 다시 집어올 가능성이 높다. **적어두는 이유가 그것이다.**

### ❌ SCIDOCS

BEIR가 이 데이터셋의 태스크를 아예 "Citation-Prediction"으로 분류한다. 라벨이 인용·공동인용·공동열람이다. 같은 순환.

### ❌ HotpotQA — 부분 순환, 상한 측정용으로만

라벨 자체는 사람이 만들었다(크라우드 워커가 질문 작성 + 근거 문장 표시). 그런데 **코퍼스 구축 절차가 문제다**: 저자들이 위키백과 첫 문단을 노드로 하이퍼링크 그래프를 만들고, **링크로 연결된 문단 쌍을 작업자에게 보여주며** 두 문단이 모두 필요한 질문을 쓰게 했다.

즉 정답 문서 쌍이 **구성상 하이퍼링크 엣지**다. 쿼리 분포 전체가 링크로 도달 가능한 것만 담고 있다. NFCorpus만큼 나쁘진 않지만 그래프 신호의 값을 부풀린다.

용도가 아예 없진 않다: **"그래프가 최대로 잘 될 때 얼마나 되나"의 상한**으로는 쓸 수 있다. 여기서도 개선이 없으면 구현이 틀린 것이다.

### ❌ Cora · CiteSeer · PubMed · WikiCS

인용/하이퍼링크 그래프가 깔끔하게 있고 크기도 작다(Cora 2,708노드/5,429엣지, WikiCS 11,701/216,213). 하지만 **쿼리도 qrels도 없다.** 노드 분류·링크 예측 벤치마크이고 IR 평가 데이터셋이 아니다. nDCG@10을 계산할 대상이 없다.

### ⚠️ ClueWeb09 + TREC Web Track — 이론상 정답, 현실적으로 불가

조건을 완벽히 만족하는 유일한 데이터셋이다:

- **사람 판정** 70,575개, 200 토픽, 4점 척도 (TREC Web 2009–2012)
- **완전한 웹 그래프** — 454,075,638 아웃링크, 3GB(비압축). Category B(영어 50.2M 페이지)에 대해서도 그래프가 완전하다
- 라벨과 그래프가 독립 — NIST 평가자가 링크를 보고 판정한 게 아니다

막는 것: **코퍼스 라이선스 계약 필요**, 전체 5TB 압축. 1인 개발자의 M4 규모가 아니다. qrels는 trec.nist.gov에서 무료지만 문서 컬렉션이 게이트되어 있다.

기록해 두는 이유: 나중에 규모가 되면 **이게 최종 목표**다.

---

## ✅ 권장 — TREC-COVID ⋈ Semantic Scholar 인용 그래프

두 출처를 문서 ID로 조인한다. **라벨과 그래프의 출처가 다르다는 것이 이 조합의 핵심이다.**

| 축 | 출처 | 독립성 |
|---|---|---|
| 라벨 | TREC-COVID qrels — 생의학 전문가·NIST 평가자의 사람 판정 | 링크를 보고 만든 게 아님 ✓ |
| 그래프 | Semantic Scholar `citations` 데이터셋 (월간 스냅샷, Datasets API) | 라벨 생성에 관여 안 함 ✓ |

**왜 TREC-COVID인가**

- **판정 밀도가 압도적이다** — 쿼리당 평균 493.5 qrels. BEIR 18개 중 최고이고 대부분은 5 미만이다.
- 이게 A/B에 결정적이다. BEIR 문서가 지적하듯 **qrels에 없는 문서는 무조건 무관련 취급**되므로, 판정이 얕으면 "관련 있지만 판정 안 된 문서"를 올린 시스템이 억울하게 낮은 점수를 받는다. 그래프 신호는 텍스트가 못 찾는 문서를 올리는 것이 존재 이유라서 **정확히 이 함정에 걸린다.** 493.5는 그 위험을 크게 줄인다.
- 171K 문서 / 50 쿼리 — 인메모리 사정권
- nDCG@10이 BEIR 주지표라 **weft의 M4 지표와 그대로 일치**한다

**차선책: DBpedia-Entity v2** — 467 쿼리, 49,280 query-entity 판정, 3점 척도, **크라우드소싱 + 이견 시 전문가 재판정**. 라벨은 사람이 만들었고 그래프는 DBpedia RDF 구조라 독립이다. 다만 코퍼스가 4.6M 엔티티이고 DBpedia 덤프를 따로 처리해야 한다. TREC-COVID가 막히면 여기로.

---

## 측정 설계에서 반드시 지킬 것

### 1. 판정 풀로 좁힐 때 이웃을 함께 넣어라

전체 코퍼스를 색인하는 대신 판정된 문서만으로 재순위(re-ranking) 평가를 하는 건 표준 관행이고 A/B에 충분하다. 하지만 **판정 풀만 남기면 판정 안 된 문서로 가는 인용 엣지가 전부 dangling이 되어 그래프 스코어러가 무력화된다.**

풀 + 풀의 1~2홉 이웃까지 색인해야 한다. 이웃은 순회에 참여하되 점수 집계에서는 미판정으로 다뤄진다.

### 2. 3-way A/B다, 2-way가 아니다

[FINDINGS 3.1](FINDINGS.md)이 남긴 요구사항:

| 구성 | 목적 |
|---|---|
| text + vector | 기준선 |
| text + vector + `graph.New` | **그래프의 진짜 기여** (시드 제외) |
| text + vector + `graph.NewIncludingSeeds` | 시드 에코가 있던 옛 동작 — 이중 계산이 얼마나 부풀렸는지 |

세 번째가 없으면 "개선"이 그래프 덕인지 텍스트 가중치 2배 덕인지 여전히 구분되지 않는다.

### 3. `ir_datasets`의 "links"는 엣지가 아니다

카탈로그의 `links`·`citations` 필드는 **문서 링크와 인용 문헌 정보**다. 코어 엔티티는 docs·queries·qrels·docpairs·scoreddocs뿐이고 **문서 간 엣지는 어떤 데이터셋에도 실려 있지 않다.** 그래프는 직접 조인해야 한다. 같은 이유로 BEIR `corpus.jsonl`도 `_id`/`title`/`text` 세 필드뿐이다.

### 4. RRF k=60을 함께 스윕하라

[FINDINGS 3.3](FINDINGS.md)에서 감쇠가 예상보다 강해 recency가 순서를 못 바꿨다. `k`를 고정한 채 그래프 기여를 재면 그래프가 아니라 `k=60`을 측정할 위험이 있다.

---

## 마일스톤 2에 주는 확인

이 조사가 M2 범위를 바꿀 수 있어서 순서를 앞당겼다. 결과는 **바꿀 이유 없음**이고, 오히려 기존 설계 두 개를 검증했다:

- **`Document.Links`를 Key 기반으로 유지하라** ([FINDINGS 5절 권고 2](FINDINGS.md)) — 권장 경로가 바로 "문서 ID로 외부 인용 그래프를 조인"하는 것이다. 인용 대상의 상당수는 코퍼스 밖에 있어 **dangling으로 남는다.** weft는 이미 이걸 설계로 처리한다(`TestDanglingLinksAreIgnored`). DocID 기반 인접 리스트로 굳혔다면 여기서 막혔다.
- **그래프는 1급으로 영속화한다** — 폐기 근거가 없다. 마일스톤 4가 실행 가능하다.

---

## 출처

- [BEIR / BeIR corpus (Hugging Face)](https://huggingface.co/datasets/BeIR/beir-corpus) · [Elastic: BEIR 데이터셋 통계표](https://www.elastic.co/search-labs/blog/evaluating-search-relevance-part-1)
- [NFCorpus (StatNLP Heidelberg)](https://www.cl.uni-heidelberg.de/statnlpgroup/nfcorpus/) · [NFCorpus (ir_datasets)](https://ir-datasets.com/nfcorpus.html) · [mteb/nfcorpus](https://huggingface.co/datasets/mteb/nfcorpus)
- [DBpedia-Entity v2 공식](https://iai-group.github.io/DBpedia-Entity/) · [GitHub](https://github.com/iai-group/DBpedia-Entity) · [SIGIR'17](https://dl.acm.org/doi/10.1145/3077136.3080751)
- [ClueWeb09 (Lemur Project)](https://lemurproject.org/clueweb09.php/) · [TREC 2013 Web Track Overview](https://trec.nist.gov/pubs/trec22/papers/WEB.OVERVIEW.pdf) · [TREC 2014 Web Track Overview](https://trec.nist.gov/pubs/trec23/papers/overview-web.pdf)
- [HotpotQA (Yang et al., 2018)](https://nlp.stanford.edu/pubs/yang2018hotpotqa.pdf)
- [ir_datasets (MacAvaney et al., SIGIR 2021)](https://arxiv.org/pdf/2103.02280) · [Semantic Scholar Open Data Platform](https://arxiv.org/pdf/2301.10140)
