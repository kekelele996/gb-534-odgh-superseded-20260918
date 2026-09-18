package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"fermentation-kinetics-deviation-analysis/backend/internal/algorithm"
	"fermentation-kinetics-deviation-analysis/backend/internal/constants"
	"fermentation-kinetics-deviation-analysis/backend/internal/dto"
	"fermentation-kinetics-deviation-analysis/backend/internal/model"
	"fermentation-kinetics-deviation-analysis/backend/internal/repository"
	"fermentation-kinetics-deviation-analysis/backend/internal/timeseries"
	"fermentation-kinetics-deviation-analysis/backend/internal/util"

	"gorm.io/gorm"
)

func TestAnalysisIdempotencyThreeRoleSeparationAndReplay(t *testing.T) {
	db := newTestDB(t)
	vesselRepo := repository.NewFermentationVesselRepository(db)
	recipeRepo := repository.NewCultureRecipeRepository(db)
	seriesRepo := repository.NewSensorSeriesRepository(db)
	analysisRepo := repository.NewDeviationAnalysisRepository(db)
	auditRepo := repository.NewAuditRepository(db)
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	vessel := model.FermentationVessel{
		VesselCode: "FV-A1", Name: "Analysis vessel", WorkingVolumeL: 100,
		SensorChannels: `["ph"]`, Location: "Lab", OwnerTeam: "Process",
		VesselState: "active", CommissionedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := vesselRepo.Create(context.Background(), &vessel); err != nil {
		t.Fatal(err)
	}
	boundaries, references, tolerances := testRecipeConfig(t)
	recipe := model.CultureRecipe{
		VesselID: vessel.ID, RecipeCode: "ANALYSIS-A", Version: 1, Organism: "Test organism",
		TargetDurationH: 8, PhaseBoundariesJSON: string(boundaries), ReferenceCurvesJSON: string(references),
		ToleranceProfileJSON: string(tolerances), RecipeState: "published",
		CreatedBy: 8, CreatedByName: "scientist", CreatedAt: now, UpdatedAt: now,
	}
	if err := recipeRepo.Create(context.Background(), &recipe); err != nil {
		t.Fatal(err)
	}
	points := make([]timeseries.Point, 0, 9)
	for hour := 0; hour <= 8; hour++ {
		value := 7 - float64(hour)*0.05
		valueCopy := value
		points = append(points, timeseries.Point{
			Timestamp: now.Add(time.Duration(hour) * time.Hour), Values: map[string]*float64{"ph": &valueCopy},
		})
	}
	pointsJSON, err := timeseries.EncodePoints(points)
	if err != nil {
		t.Fatal(err)
	}
	series := model.SensorSeries{
		VesselID: vessel.ID, RecipeID: recipe.ID, RunCode: "RUN-A1", Channel: "ph",
		SampleIntervalS: 3600, PointsJSON: pointsJSON, StartedAt: now, EndedAt: now.Add(8 * time.Hour),
		SourceChecksum: util.HashString(pointsJSON), SeriesState: "ready", QualitySummary: `{"valid":true}`,
		NormalizationJSON: `{"method":"median_iqr"}`, ImportedBy: 9, ImportedByName: "analyst",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := seriesRepo.Create(context.Background(), &series); err != nil {
		t.Fatal(err)
	}
	svc := NewDeviationAnalysisService(analysisRepo, recipeRepo, seriesRepo, auditRepo, algorithm.NewEvaluator())
	initiator := util.Actor{UserID: 9, Username: "analyst", Role: "data_analyst", RequestID: "req-run"}
	first, reused, err := svc.Run(context.Background(), dto.RunDeviationAnalysisRequest{SensorSeriesID: series.ID}, "idem-a", initiator)
	if err != nil || reused {
		t.Fatalf("first run reused=%v err=%v", reused, err)
	}
	second, reused, err := svc.Run(context.Background(), dto.RunDeviationAnalysisRequest{SensorSeriesID: series.ID}, "idem-a", initiator)
	if err != nil || !reused || second.ID != first.ID {
		t.Fatalf("same-key run id=%d reused=%v err=%v", second.ID, reused, err)
	}
	third, reused, err := svc.Run(context.Background(), dto.RunDeviationAnalysisRequest{SensorSeriesID: series.ID}, "idem-b", initiator)
	if err != nil || !reused || third.ID != first.ID {
		t.Fatalf("same-input run id=%d reused=%v err=%v", third.ID, reused, err)
	}
	ctx := context.Background()
	reviewer := util.Actor{UserID: 10, Username: "reviewer", Role: "reviewer", RequestID: "req-review"}
	confirmer := util.Actor{UserID: 11, Username: "qa-lead", Role: "reviewer", RequestID: "req-confirm"}

	// The initiator cannot review their own analysis either.
	_, err = svc.Transition(ctx, first.ID, dto.DeviationAnalysisTransitionRequest{
		ToState: "reviewed", Comment: "Self review must fail.",
	}, initiator)
	if !expectAppError(err, util.CodeReviewerConflict) {
		t.Fatalf("self review error=%v, want reviewer conflict", err)
	}

	reviewed, err := svc.Transition(ctx, first.ID, dto.DeviationAnalysisTransitionRequest{
		ToState: "reviewed", Comment: "Evidence reviewed.",
	}, reviewer)
	if err != nil {
		t.Fatalf("review transition: %v", err)
	}
	if reviewed.ReviewedBy == nil || *reviewed.ReviewedBy != reviewer.UserID || reviewed.ReviewedAt == nil {
		t.Fatal("review did not persist reviewer identity and time")
	}

	// The initiator still cannot confirm.
	_, err = svc.Transition(ctx, first.ID, dto.DeviationAnalysisTransitionRequest{
		ToState: "confirmed", Comment: "Self confirmation must fail.",
	}, initiator)
	if !expectAppError(err, util.CodeReviewerConflict) {
		t.Fatalf("self-confirm error=%v, want reviewer conflict", err)
	}
	// The person who marked it reviewed cannot confirm.
	_, err = svc.Transition(ctx, first.ID, dto.DeviationAnalysisTransitionRequest{
		ToState: "confirmed", Comment: "Reviewer confirmation must fail.",
	}, reviewer)
	if !expectAppError(err, util.CodeReviewerConflict) {
		t.Fatalf("reviewer confirm error=%v, want reviewer conflict", err)
	}
	confirmed, err := svc.Transition(ctx, first.ID, dto.DeviationAnalysisTransitionRequest{
		ToState: "confirmed", Comment: "Independent confirmation.",
	}, confirmer)
	if err != nil {
		t.Fatalf("independent third-person confirm: %v", err)
	}
	if confirmed.ConfirmedBy == nil || *confirmed.ConfirmedBy != confirmer.UserID || confirmed.ConfirmedAt == nil {
		t.Fatal("confirmation did not persist confirmer identity and time in one write")
	}

	// Duplicate confirmation must not succeed a second time (illegal transition
	// once the row has left "reviewed"/"confirmed" terminal for confirmation).
	_, err = svc.Transition(ctx, first.ID, dto.DeviationAnalysisTransitionRequest{
		ToState: "confirmed", Comment: "Duplicate confirmation must fail.",
	}, confirmer)
	var appErr *util.AppError
	if !errors.As(err, &appErr) || appErr.Code != util.CodeStateTransition {
		t.Fatalf("duplicate confirm error=%v, want invalid state transition", err)
	}

	replayed, err := svc.Replay(ctx, first.ID, reviewer)
	if err != nil || replayed.ReplayVerified == nil || !*replayed.ReplayVerified {
		t.Fatalf("replay verified=%v err=%v", replayed.ReplayVerified, err)
	}

	// Available actions reflect the viewer identity.
	if len(confirmed.AvailableActions) != 1 || confirmed.AvailableActions[0] != dto.ActionVoid {
		t.Fatalf("confirmed analysis actions=%v, want only void", confirmed.AvailableActions)
	}
}

func TestReturnToInvestigationClearsReviewAndRestoresEligibility(t *testing.T) {
	db := newTestDB(t)
	analysisRepo := repository.NewDeviationAnalysisRepository(db)
	recipeRepo := repository.NewCultureRecipeRepository(db)
	seriesRepo := repository.NewSensorSeriesRepository(db)
	auditRepo := repository.NewAuditRepository(db)
	svc, id, ctx := reviewedAnalysisFixture(t, db, analysisRepo, recipeRepo, seriesRepo, auditRepo)
	reviewer := util.Actor{UserID: 10, Username: "reviewer", Role: "reviewer", RequestID: "req-return"}
	confirmer := util.Actor{UserID: 11, Username: "qa-lead", Role: "reviewer", RequestID: "req-confirm"}

	// A return reason is mandatory.
	_, err := svc.Transition(ctx, id, dto.DeviationAnalysisTransitionRequest{
		ToState: "investigating", Comment: "   ",
	}, reviewer)
	if !expectAppError(err, util.CodeValidation) {
		t.Fatalf("blank return reason error=%v, want validation error", err)
	}

	returned, err := svc.Transition(ctx, id, dto.DeviationAnalysisTransitionRequest{
		ToState: "investigating", Comment: "pH probe calibration looks inconsistent.",
	}, reviewer)
	if err != nil {
		t.Fatalf("return transition: %v", err)
	}
	if returned.AnalysisState != "investigating" {
		t.Fatalf("state=%s, want investigating", returned.AnalysisState)
	}
	if returned.ReviewedBy != nil || returned.ReviewedAt != nil || returned.ReviewComment != "" {
		t.Fatal("return to investigation did not clear the original review conclusion")
	}
	if returned.ReturnReason != "pH probe calibration looks inconsistent." {
		t.Fatalf("return reason=%q not preserved", returned.ReturnReason)
	}
	// With no current reviewer, confirmation must still be blocked (no reviewed state).
	if _, err := svc.Transition(ctx, id, dto.DeviationAnalysisTransitionRequest{
		ToState: "confirmed",
	}, confirmer); !expectAppError(err, util.CodeStateTransition) {
		t.Fatalf("confirm while investigating error=%v, want state transition error", err)
	}

	// A different reviewer records a fresh conclusion; eligibility is restored.
	secondReviewer := util.Actor{UserID: 12, Username: "second-reviewer", Role: "process_scientist", RequestID: "req-rereview"}
	reReviewed, err := svc.Transition(ctx, id, dto.DeviationAnalysisTransitionRequest{
		ToState: "reviewed", Comment: "Re-checked against calibrated probe; evidence holds.",
	}, secondReviewer)
	if err != nil {
		t.Fatalf("re-review: %v", err)
	}
	if reReviewed.ReviewedBy == nil || *reReviewed.ReviewedBy != secondReviewer.UserID || reReviewed.ReturnReason != "" {
		t.Fatal("re-review did not replace reviewer and clear return reason")
	}
	// The first reviewer is no longer the recorded reviewer and may now confirm.
	confirmed, err := svc.Transition(ctx, id, dto.DeviationAnalysisTransitionRequest{
		ToState: "confirmed", Comment: "Confirmed after re-investigation.",
	}, reviewer)
	if err != nil {
		t.Fatalf("confirm after re-review: %v", err)
	}
	if confirmed.ConfirmedBy == nil || *confirmed.ConfirmedBy != reviewer.UserID {
		t.Fatal("confirmer was not written after re-review restored eligibility")
	}
}

func TestFailedTransitionLeavesStateReviewAndConfirmationUntouched(t *testing.T) {
	db := newTestDB(t)
	analysisRepo := repository.NewDeviationAnalysisRepository(db)
	recipeRepo := repository.NewCultureRecipeRepository(db)
	seriesRepo := repository.NewSensorSeriesRepository(db)
	auditRepo := repository.NewAuditRepository(db)
	svc, id, ctx := reviewedAnalysisFixture(t, db, analysisRepo, recipeRepo, seriesRepo, auditRepo)
	reviewer := util.Actor{UserID: 10, Username: "reviewer", Role: "reviewer", RequestID: "req-fail"}

	before, err := analysisRepo.GetByID(ctx, id, false)
	if err != nil {
		t.Fatal(err)
	}
	// Illegal transition (reviewed -> failed) must change nothing.
	if _, err := svc.Transition(ctx, id, dto.DeviationAnalysisTransitionRequest{
		ToState: "failed", Comment: "impossible",
	}, reviewer); !expectAppError(err, util.CodeStateTransition) {
		t.Fatalf("illegal transition error=%v, want state transition error", err)
	}
	after, err := analysisRepo.GetByID(ctx, id, false)
	if err != nil {
		t.Fatal(err)
	}
	if after.AnalysisState != before.AnalysisState || after.ReviewComment != before.ReviewComment ||
		after.ConfirmedBy != before.ConfirmedBy || after.UpdatedAt != before.UpdatedAt {
		t.Fatal("a rejected transition modified state, review or confirmation data")
	}
}

func TestConfirmConditionalUpdateGuardsConcurrentChange(t *testing.T) {
	db := newTestDB(t)
	analysisRepo := repository.NewDeviationAnalysisRepository(db)
	now := time.Date(206, 8, 20, 0, 0, 0, 0, time.UTC)
	analysis := model.DeviationAnalysis{
		SensorSeriesID: 1, RecipeID: 1, RecipeVersion: 1, AlgorithmVersion: algorithm.Version,
		InputHash: "hash-concurrent", InputSnapshot: "{}", PhaseScoresJSON: "[]",
		DeviationLevel: "normal", AlignedCurveJSON: "[]", SuspectedCausesJSON: "[]",
		AnalysisState: "reviewed", Explanation: "x", AnalyzedAt: now, InitiatedBy: 9,
		InitiatedByName: "analyst", IdempotencyKey: "idem-concurrent", CreatedAt: now, UpdatedAt: now,
	}
	if err := analysisRepo.Create(context.Background(), &analysis); err != nil {
		t.Fatal(err)
	}
	// Move the row to confirmed out-of-band, then attempt a confirm guarded on "reviewed".
	changed, err := analysisRepo.Transition(context.Background(), analysis.ID, "reviewed", "confirmed", nil)
	if err != nil || !changed {
		t.Fatalf("seed concurrent confirm changed=%v err=%v", changed, err)
	}
	err = analysisRepo.TransitionWithAudit(context.Background(), analysis.ID, "reviewed", "confirmed",
		map[string]any{"confirmed_by": uint(11)}, model.AuditLog{})
	if !errors.Is(err, repository.ErrAnalysisStateChanged) {
		t.Fatalf("concurrent confirm error=%v, want ErrAnalysisStateChanged", err)
	}
	// The losing transaction must not have written its confirmer or an audit row.
	stored, err := analysisRepo.GetByID(context.Background(), analysis.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ConfirmedBy != nil {
		t.Fatalf("losing confirm wrote confirmed_by=%d", *stored.ConfirmedBy)
	}
	var auditCount int64
	if err := db.Model(&model.AuditLog{}).Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if auditCount != 0 {
		t.Fatalf("rolled-back confirm left %d audit rows", auditCount)
	}
}

func TestRunRequiresReadySeriesAndIdempotencyKey(t *testing.T) {
	db := newTestDB(t)
	svc := NewDeviationAnalysisService(
		repository.NewDeviationAnalysisRepository(db), repository.NewCultureRecipeRepository(db),
		repository.NewSensorSeriesRepository(db), repository.NewAuditRepository(db), algorithm.NewEvaluator(),
	)
	_, _, err := svc.Run(context.Background(), dto.RunDeviationAnalysisRequest{SensorSeriesID: 99}, "", util.Actor{})
	if !expectAppError(err, util.CodeIdempotency) {
		t.Fatalf("missing key error=%v", err)
	}
}

func TestAnalysisRoleContract(t *testing.T) {
	if constants.HasPermission(constants.RoleDataAnalyst, constants.PermissionAnalysisConfirm) {
		t.Fatal("data analyst should not receive confirm permission")
	}
}

func expectAppError(err error, code util.ErrorCode) bool {
	var appErr *util.AppError
	return errors.As(err, &appErr) && appErr.Code == code
}

// reviewedAnalysisFixture builds a completed analysis driven through review by a
// reviewer distinct from the initiator, and returns the service, analysis ID and context.
func reviewedAnalysisFixture(
	t *testing.T, db *gorm.DB,
	analysisRepo repository.DeviationAnalysisRepository,
	recipeRepo repository.CultureRecipeRepository,
	seriesRepo repository.SensorSeriesRepository,
	auditRepo repository.AuditRepository,
) (*DeviationAnalysisService, uint, context.Context) {
	t.Helper()
	ctx := context.Background()
	svc := NewDeviationAnalysisService(analysisRepo, recipeRepo, seriesRepo, auditRepo, algorithm.NewEvaluator())
	initiator := util.Actor{UserID: 9, Username: "analyst", Role: "data_analyst", RequestID: "req-run-fix"}
	run, reused, err := svc.Run(ctx, dto.RunDeviationAnalysisRequest{SensorSeriesID: fixtureReadySeries(
		t, db, recipeRepo, seriesRepo,
	)}, "idem-"+t.Name(), initiator)
	if err != nil || reused {
		t.Fatalf("fixture run reused=%v err=%v", reused, err)
	}
	reviewer := util.Actor{UserID: 10, Username: "reviewer", Role: "reviewer", RequestID: "req-review-fix"}
	if _, err := svc.Transition(ctx, run.ID, dto.DeviationAnalysisTransitionRequest{
		ToState: "reviewed", Comment: "Initial independent review.",
	}, reviewer); err != nil {
		t.Fatalf("fixture review: %v", err)
	}
	return svc, run.ID, ctx
}

// fixtureReadySeries creates vessel, published recipe and ready series for a
// test, returning the series ID.
func fixtureReadySeries(
	t *testing.T, db *gorm.DB,
	recipeRepo repository.CultureRecipeRepository, seriesRepo repository.SensorSeriesRepository,
) uint {
	t.Helper()
	ctx := context.Background()
	vesselRepo := repository.NewFermentationVesselRepository(db)
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	vessel := model.FermentationVessel{
		VesselCode: "FV-" + t.Name(), Name: "Fixture vessel", WorkingVolumeL: 100,
		SensorChannels: `["ph"]`, Location: "Lab", OwnerTeam: "Process",
		VesselState: "active", CommissionedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := vesselRepo.Create(ctx, &vessel); err != nil {
		t.Fatal(err)
	}
	boundaries, references, tolerances := testRecipeConfig(t)
	recipe := model.CultureRecipe{
		VesselID: vessel.ID, RecipeCode: "REC-" + t.Name(), Version: 1, Organism: "Test organism",
		TargetDurationH: 8, PhaseBoundariesJSON: string(boundaries), ReferenceCurvesJSON: string(references),
		ToleranceProfileJSON: string(tolerances), RecipeState: "published",
		CreatedBy: 8, CreatedByName: "scientist", CreatedAt: now, UpdatedAt: now,
	}
	if err := recipeRepo.Create(ctx, &recipe); err != nil {
		t.Fatal(err)
	}
	points := make([]timeseries.Point, 0, 9)
	for hour := 0; hour <= 8; hour++ {
		value := 7 - float64(hour)*0.05
		valueCopy := value
		points = append(points, timeseries.Point{
			Timestamp: now.Add(time.Duration(hour) * time.Hour), Values: map[string]*float64{"ph": &valueCopy},
		})
	}
	pointsJSON, err := timeseries.EncodePoints(points)
	if err != nil {
		t.Fatal(err)
	}
	series := model.SensorSeries{
		VesselID: vessel.ID, RecipeID: recipe.ID, RunCode: "RUN-" + t.Name(), Channel: "ph",
		SampleIntervalS: 3600, PointsJSON: pointsJSON, StartedAt: now, EndedAt: now.Add(8 * time.Hour),
		SourceChecksum: util.HashString(pointsJSON), SeriesState: "ready", QualitySummary: `{"valid":true}`,
		NormalizationJSON: `{"method":"median_iqr"}`, ImportedBy: 9, ImportedByName: "analyst",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := seriesRepo.Create(ctx, &series); err != nil {
		t.Fatal(err)
	}
	return series.ID
}
