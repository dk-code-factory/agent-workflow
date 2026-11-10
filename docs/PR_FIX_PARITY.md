# PR-fix 5.2.0 parity and deterministic evidence

The source contract is pinned under `pkg/prfix/source/`; provenance is verified
by `TestPinnedSourceProvenance`. Tests use local fakes and disposable repositories.
No live-provider, login-refresh, publication, repository creation, or merge
conformance evidence is claimed.

| Obligation | YAML / application operations | Deterministic evidence |
| --- | --- | --- |
| Source default task order and finalizers | generated `pass.yml`, `expand` | `TestDefaultOrderAndNestedReviewerAdapters`, `TestDefaultPlanExecutesAllDutiesWithFakeCIAndCleanup` |
| Custom and empty replacement lists; always managed completion | preparation, `completion.yml` | `TestPrepareEmptyPlanIsInspectableAndStillHasCompletion`, `TestEmptyPlanExecutesManagedLiveGateWithSameSessionFollowup` |
| No default reviewer coverage | adapter preflight | `TestDefaultPlanBlocksMissingReviewersBeforeClaims` |
| Every custom identity has a bounded adapter | adapter registry, selected-role preflight | `TestUnavailableOptionalAndUnsupportedControlsBlock`, `TestAdapterIdentityRoleAndOwnershipBlockBeforeDispatch`, `TestDefaultOrderAndNestedReviewerAdapters` |
| Distinct repeated occurrences, required passes, parallel barriers | materialized jobs, `occurrence.yml`, `claim` | `TestRepeatedParallelReviewPassesHaveDistinctClaims` |
| Same-session checklist and explicit restart recovery | `attempt.yml`, `check.yml`, `turn.yml`; worker lifecycle actions | `TestStoppedChecklistWorkerRecoversOriginalSessionAcrossLostCheckpoints` |
| Optional versus required incomplete native reports | `finish`, `status` | `TestOptionalAndRequiredIncompleteReports` |
| Eligible retry retains cycle and original failure | occurrence repeat, transactional reservation | `TestUsageRetryStaysInSameCycleAndPreservesNativeFailure` |
| Settled optional failures and eligible retries finish in the same execution; unsafe failures retain barriers | `attempt.yml`, `worker_acknowledge`, controller-only worker API | `TestOptionalNativeFailureCompletesWithOriginalEvidence`, `TestLostFailureAcknowledgementCheckpointRetainsRetry`, `TestExhaustedNativeRetryRetainsUnacknowledgedFailure`, `TestOptionalNativeFailureCannotAcknowledgeUnsafeCleanup` |
| Defaults/user/upstream/repository/invocation precedence, exact text, no store writes | embedded read-only reader | `TestSettingsScopesReplaceListsAndPreserveExactInstructions` |
| Legacy limits/hooks and ownership identities | pinned compatibility readers, capture | `TestLegacyLimitsHooksAndNumericPersonalAccounts` |
| Resource pinning and content/history freshness | snapshot and occurrence fingerprints | `TestResourcesPinDirectoryContentsAndRejectCycles`, `TestContentFingerprintPreservesHistoryOnlyCleanup`, `TestLaterConfiguredWorkFinishesBeforeAffectedReviewRepeats` |
| Logical identity, retained budgets/deadlines, exhausted continuation deduplication | application SQLite ledger | `TestRetainedBudgetsContinuationAndDeadlines`, `TestReplacementRetainsBudgetsSiblingsAndReconcilesLostPublication`, `TestCompletedUnchangedResumeRetainsOriginalAdmission` |
| Lost lifecycle and completion checkpoints | registered action reconciliation | `TestLostLifecycleCheckpointsReconcileWithoutDuplicateWorkers` |
| Lost publication responses and receipt checkpoints, no duplicate writes | `publication.yml`, `publish`, `receipt` | `TestUncertainPublicationAndReceiptCheckpointNeverDuplicateWrite` |
| Configuration cannot authorize writes or merge | intent admission, publication gate | `TestNoAuthorityCannotPreparePublication` |
| Focused commits preserve unrelated workspace files | manifest-bound `commit` action | `TestFocusedCommitPreservesUnrelatedFiles` |
| Exact-head CI logs and exact-attempt flake/exception proof | `ci.yml`, `ci`, `ci_classify`, provider verification | `TestCIObservationRetainsExactFailureLogs`, `TestGitHubExactAttemptFlakeAndExceptionVerification`, `TestCIRepairCycleZeroReturnsThroughSelectedCleanup`, `TestLostCIRerunResponseReconcilesWithoutSecondRerun` |
| Scope/series assessments, replies, split publication, rerun and merge gates | typed reports, scope/cleanup validation, publication guards | `TestCleanupRequiresWholeSeriesTreeAndVerifiedPublication`, `TestScopeUsesFinalDiffAndExactEffectiveModes`, `TestSplitPublicationUsesDisposableLocalGitAndReconcilesLostPush`, `TestMergeRequiresSeparateAuthorityAndLiveRepositoryRequirements`, `TestReplyAndNoReplyObligationsRefreshWithoutDuplicateAnswer`, `TestRequiredNoReplyNeedsExactUserException` |
| Original findings survive rechecks and need sourced dispositions | original native messages, `verification.yml`, feedback fixes | `TestOriginalFindingSurvivesEmptyChecklistOutputAndGetsSourcedDisposition` |
| Selected completion differs from PR readiness | managed gate plus live `completion` action | Empty-plan and focused-commit execution tests |
| Parallel artifact publication and interrupted writers | synced temporary artifacts, atomic no-replace publication | `TestImmutablePublicationConcurrentAndInterruptedWriters` |
| Production workspace scoping retains session recovery | managed CLI runner workspace wrapper | `TestManagedRunnerWorkspaceRecoveryUsesProductionRestore` |
| Reviewer-local finding IDs cannot collide | claim-scoped finding identities | `TestIndependentReviewerLocalIDsRetainEveryOriginalObligation` |
| Nested selection drift blocks dispatch | live configuration gate | `TestNestedReviewerConfigurationDriftBlocksBeforeDispatch` |
| Hooks require cycle admission and follow selected work | generated admission/conditional hook jobs | `TestCycleHooksAdmittedBeforeEffectsAndFollowAllSelectedWork`, `TestCycleHookReuseRequiresNoNewReservation` |
| Verified whole commit-policy handoff | bounded provider reports and cleanup inputs | `TestCommitPolicyProviderRequiresAndHandsOffOriginalInstructions`, `TestCommitPolicyLiteralBuiltinAndInstructionOnlyPreparation`, `TestCleanupRetainsOnlyItsVerifiedHistoryChange` |
| Merge outcome matches actor, target and method | read-only merge commit reconciliation | `TestMergeReconciliationRequiresExactActorMethodAndTargetParents` |
| Resolved CI polling cadence | exact-head CI observation | `TestCIPollIntervalResolvesConfigurationAndTestOverride` |
| Every task/finalizer review block has admission, hooks and verification | generated `pass.yml` barriers | `TestEveryReviewBlockAndFinalizerReviewGetsVerification` |
| Fresh noncacheable passes consume fresh cycles; hooks bind to their cycle | stable engine operation identities, claim fingerprints | `TestFreshPassCyclesCannotReuseOneAllowance`, `TestCacheableHooksBindTheirNewReviewCycle` |
| Concurrent controllers cannot dispatch the same publication twice | cancellable application SQLite publication lock | `TestConcurrentControllersDispatchOnePublication`, `TestPublicationLockCancellationAndRelease` |
| Split creation and CI reruns require exact observed outcomes | read-only provider reconciliation | `TestSplitPRReconciliationRequiresExactCreationIdentity`, `TestCIRerunReconciliationRequiresExactlyNextAttempt` |
| Historical writes cannot hide incoming thread reopening | governing snapshot and completed-effect provenance | `TestHistoricalResolutionDoesNotHideIncomingReopening` |
| CI classification obeys its retained observation controls | bounded classification context | `TestCIClassificationHonorsItsConfiguredDeadline` |
| Failed candidate reads retain their original diagnostic | `candidates` action | `TestCandidateReadFailureRetainsItsDiagnostic` |
| Conflicting singleton procedures reject before dispatch | parallel selection preflight | `TestParallelSingletonProceduresRejectBeforeClaims` |
| Inactive hooks do not consume first-pass worker admission | reachable generated groups | `TestInactiveHooksDoNotConsumeWorkerAdmission` |
| Exact instructions fit both initial and same-session checklist prompts | claim admission and checklist envelope | `TestChecklistEnvelopeAdmissionCoversSameSessionBoundary` |
| Failed or incomplete optional outputs cannot clear findings | accepted-claim disposition gate | `TestUnacceptedOptionalFixCannotDisposeOriginalFinding` |
| Unaccepted inspections cannot weaken retained reply requests | accepted obligation revision gate | `TestUnacceptedInspectionCannotWeakenRetainedReplyObligation` |
| Conditional decisions preserve matched preference scope | live preference acceptance | `TestConditionalPreferenceDecisionsRetainMatchedPaths` |
| CI reads are bounded and retain extension history | CI observation transport and deadline ledger | `TestCITransportUsesRetainedObservationDeadline`, `TestCITransportExtensionSurvivesInterruptedObservation` |
| CI fixture failures return controlled evidence errors | test report helper and guarded callbacks | `TestCIReportFixtureReturnsMissingAndMalformedEvidence` |
| Existing workflow/engine compatibility | CLI actions plus authorized worker acknowledgement API; no workflow schema or database migration | `make test`, `make check`, `make ui-pr-check BASE=origin/main` |
| Unknown non-test CI failures cannot inherit test exemptions | exact-attempt failure inventory | `TestFlakeCannotExemptUnrelatedBuildFailure` |
| Rerun attempts are integral before any write; verified attempts retain separate windows | intent admission, publication receipt transaction, CI deadlines | `TestFractionalRerunIsRejectedBeforeIntentOrDispatch`, `TestVerifiedRerunOpensOneRetainedAttemptWindow` |
| Live policy opt-outs require replacement preparation | full selection drift gate | `TestPartitionOptOutBlocksNewAudienceClaim` |
| Merge includes the clean reviewed local head | live merge publication gate | `TestMergeCannotOmitReviewedUnpublishedHead` |
| Unsupported CI duration ranges reject before claims | adapter preflight and runtime validation | `TestUnsupportedCIDurationBlocksBeforeClaims` |
| Recovery tests terminate locally after worker-service expiry | bounded `resumeFixture` calls | `TestStoppedChecklistWorkerRecoversOriginalSessionAcrossLostCheckpoints`, `TestLostFailureAcknowledgementCheckpointRetainsRetry`, `TestLostCIRerunResponseReconcilesWithoutSecondRerun` |
| Mutating reports recapture governing resources and implementation preferences | accepted report gate, scoped fingerprints | `TestMutatingClaimsRecaptureTheirResourcesAndPreferences` |
| Reviewer-group custom prompts reach all bounded children | expanded occurrence context, native assignment | `TestNestedReviewerGroupPromptsReachEveryLeaf`, `TestOptionalReviewerGroupRetainsChildFailureAsOmission`, `TestReviewerGroupCannotSilentlyIgnoreCoordinatorControls` |
| CI test exemptions account for all supported log output | conservative failure inventory | `TestTestExemptionAccountsForEveryLogLine`, `TestFailedStepInventoryRequiresEveryExactStep` |
| Public payloads contain no generated private operation markers | exact native payload, private creation baselines | `TestCreationPayloadsKeepPrivateIdentityOutOfPublicBodies`, `TestReplyRecoveryRequiresUniquePostBaselineCreation`, `TestSplitPRReconciliationRequiresExactCreationIdentity` |
| Report collection expansion is bounded before typed decoding | streaming shape admission | `TestReportCollectionLimitsPrecedeTypedAllocation` |
| Split recovery and publication-test failure cleanup are locally bounded | bounded recovery helper, deferred publisher joins | `TestSplitPublicationUsesDisposableLocalGitAndReconcilesLostPush`, `TestConcurrentControllersDispatchOnePublication` |
| Optional failures cannot create review or merge coverage | accepted current review count, live readiness/merge gates | `TestFailedOptionalReviewsCannotEstablishMergeCoverage` |
| GitHub integer identities survive admission, reconciliation and storage | exact JSON integer tokens and canonical ID comparison | `TestGitHubIntegerIDsSurviveCreationAndStoredEvidence`, `TestMutatingClaimsRecaptureTheirResourcesAndPreferences` |
| Resource growth cannot bypass the read budget | bounded reader and actual byte accounting | `TestResourceGrowthCannotBypassByteBudget` |
| Every writable occurrence receives implementation preferences | assignment, conditional decision and drift gates | `TestMutatingClaimsRecaptureTheirResourcesAndPreferences`, `TestConditionalPreferenceDecisionsRetainMatchedPaths` |
| Root invocation instructions and scoped repository guidance are retained | configured root call, leaf assignment, independent instruction digests | `TestRootInvocationInstructionsReachManagedLeaves`, `TestRepositoryInstructionsArePinnedOutsideContentDiff` |
| Unsupported top-level native controls block before dispatch | deterministic coordinator preflight | `TestRootNativeControlsBlockBeforeClaims`, `TestWritableReviewerBlocksBeforeClaims` |
| Test failure paths retain local bounds and worker terminal persistence | bounded frozen-clock calls, error-returning callback I/O | `TestCITransportExtensionSurvivesInterruptedObservation`, `TestVerifiedRerunOpensOneRetainedAttemptWindow`, `TestMutatingClaimsRecaptureTheirResourcesAndPreferences` |
| Setup failure cannot leave an application mailbox wait pending | worker status before mailbox observation, original failure retained | `TestQueuedWorkerSetupFailureStopsApplicationWait` |
| Flake exemptions need exact repository identity and affirmative scoped proof | live issue identity and explicit declaration checks | `TestFlakeIssueRequiresExactRepositoryAndAffirmativeScope` |
| Prepared execution inherits the saved configuration unless explicitly overridden | CLI configuration resolution before controller admission | `TestPRFixPreparedRunInheritsConfigurationAndHonorsOverride` |
| Equal-size code changes stale accepted split boundaries | current scope occurrence fingerprint before publication | `TestSplitBoundaryRejectsSameSizeContentDrift` |
| Same-cycle hook receipts require current own-resource evidence | retained cycle and snapshot fingerprint at live coverage | `TestRequiredHookResourcesStayFreshWithinSameCycle` |
| Literal filenames cannot expand focused commit ownership | literal Git add, exact blob reads and whole-index path check | `TestFocusedCommitUsesLiteralManifestPaths` |
| Untracked snapshots cannot allocate entire oversized artifacts | bounded streaming hash and aggregate admission | `TestUntrackedSnapshotRejectsOversizedSparseFile` |
| Alternate reply IDs retain one original effect and receipt | transactional semantic aliases, canonical receipt recovery | `TestAlternateReplyIDsShareOneRetainedEffect` |
| Previously verified replies permit separately authorized same-body replacement | `reply-recovery.yml`, exact inspection/reassessment, transactional link and ordinary audience publication | `TestAuthorizedSameBodyReplyReplacementIsLinkedAndReplaySafe`, `TestReplyReplacementRejectsUncertainOriginalAndMissingAuthority`, `TestGitHubReplyInspectionDistinguishesMissingEditedAndInaccessible` |
| Equivalent custom finalizers can supply current readiness evidence | admitted finalizer adapter kinds, selected coverage and exact-head CI observation | `TestEquivalentCustomCIFinalizerCanEstablishReadiness` |
| Uncertain review requests support inspected no-resend dispositions without delivery receipts | `review-request-recovery.yml`, inspection/disposition actions and live freshness gates | `TestUncertainReviewRequestNoResendRecoveryAndLostCheckpoint`, `TestGitHubReviewRequestInspectionRetainsExactHistory` |
| Merge readiness retains both REST and GraphQL predicates | production `MergeReady` observation | `TestGitHubMergeReadinessRequiresBothPredicates` |
| Immutable-writer fixture timeout joins producers before cleanup | bounded subprocess killed and joined before fixture removal | `TestImmutablePublicationConcurrentAndInterruptedWriters` |

## Authorized core acknowledgement API

`WorkerManager.AcknowledgeFailure` is controller-only and is not exposed to native
worker tools. It validates the exact run owner and generation, active admission,
message identity, actual worker join, terminal failure, empty pending mailbox,
cleaned worker invocations, and settled tool effects in the receipt transaction.
Storage, integrity, authentication, cancellation, timeout, prerequisite, and
uncertain-cleanup failures cannot be acknowledged. Closing the worker prevents
implicit dispatch after acknowledgement.

The failed message, original output, and failure class remain unchanged. The
receipt and `worker.failure_acknowledged` event are atomic and idempotent; a
conflicting request is rejected. `StopRun` recognizes receipts for the exact
failure and generation. Other failures retain their barrier. The application
checks the accepted optional disposition or remaining eligible retry allowance
before invoking the API; YAML owns the subsequent retry and selected work.

Core evidence: `TestAcknowledgeFailurePreservesEvidenceAndDeduplicates`,
`TestFailureAcknowledgementRejectsUnsafeAndStaleRequests`,
`TestFailureAcknowledgementRequiresSettledInvocationAndToolEffects`,
`TestFailureAcknowledgementWaitsForActualWorkerJoin`, and
`TestFailureAcknowledgementRollsBackReceiptFailure`.
