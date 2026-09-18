package service

import (
	"context"
	"errors"
	"fmt"
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

func TestAnalysisIdempotencyReviewerSeparationAndReplay(t *testing.T) {
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
	reviewer := util.Actor{UserID: 10, Username: "reviewer", Role: "reviewer", RequestID: "req-review"}
	if _, err := svc.Transition(context.Background(), first.ID, dto.DeviationAnalysisTransitionRequest{
		ToState: "reviewed", Comment: "Evidence reviewed.",
	}, reviewer); err != nil {
		t.Fatalf("review transition: %v", err)
	}
	// Initiator cannot confirm their own result.
	_, err = svc.Transition(context.Background(), first.ID, dto.DeviationAnalysisTransitionRequest{
		ToState: "confirmed", Comment: "Self confirmation must fail.",
	}, initiator)
	var appErr *util.AppError
	if !errors.As(err, &appErr) || appErr.Code != util.CodeDutySeparation {
		t.Fatalf("self-confirm error=%v, want duty separation", err)
	}
	// The reviewer who marked it reviewed cannot confirm it either.
	_, err = svc.Transition(context.Background(), first.ID, dto.DeviationAnalysisTransitionRequest{
		ToState: "confirmed", Comment: "Reviewer confirmation must fail.",
	}, reviewer)
	if !errors.As(err, &appErr) || appErr.Code != util.CodeDutySeparation {
		t.Fatalf("reviewer-confirm error=%v, want duty separation", err)
	}
	// A third independent person confirms; confirmer, reviewer and initiator differ.
	confirmer := util.Actor{UserID: 11, Username: "admin", Role: "admin", RequestID: "req-confirm"}
	confirmed, err := svc.Transition(context.Background(), first.ID, dto.DeviationAnalysisTransitionRequest{
		ToState: "confirmed", Comment: "",
	}, confirmer)
	if err != nil {
		t.Fatalf("independent confirm: %v", err)
	}
	if confirmed.ConfirmedBy == nil || *confirmed.ConfirmedBy != 11 || confirmed.ConfirmedAt == nil {
		t.Fatalf("confirmation fields missing: %+v", confirmed)
	}
	// Duplicate confirmation does not succeed a second time.
	_, err = svc.Transition(context.Background(), first.ID, dto.DeviationAnalysisTransitionRequest{
		ToState: "confirmed",
	}, confirmer)
	if !errors.As(err, &appErr) || appErr.Code != util.CodeStateTransition {
		t.Fatalf("duplicate confirm error=%v, want invalid state transition", err)
	}
	replayed, err := svc.Replay(context.Background(), first.ID, reviewer)
	if err != nil || replayed.ReplayVerified == nil || !*replayed.ReplayVerified {
		t.Fatalf("replay verified=%v err=%v", replayed.ReplayVerified, err)
	}
}

func TestThreeWaySeparationReturnClearsAndRereviewRestores(t *testing.T) {
	svc, analysisID := setupWorkflowAnalysis(t)
	ctx := context.Background()
	initiator := util.Actor{UserID: 9, Username: "analyst", Role: "data_analyst", RequestID: "req-init"}
	reviewer := util.Actor{UserID: 10, Username: "reviewer", Role: "reviewer", RequestID: "req-review"}
	confirmer := util.Actor{UserID: 11, Username: "qa-lead", Role: "admin", RequestID: "req-confirm"}

	// Initiator cannot review their own analysis.
	_, err := svc.Transition(ctx, analysisID, dto.DeviationAnalysisTransitionRequest{
		ToState: "reviewed", Comment: "self review",
	}, initiator)
	var appErr *util.AppError
	if !errors.As(err, &appErr) || appErr.Code != util.CodeDutySeparation {
		t.Fatalf("self review error=%v, want duty separation", err)
	}
	reviewed, err := svc.Transition(ctx, analysisID, dto.DeviationAnalysisTransitionRequest{
		ToState: "reviewed", Comment: "Looks consistent.",
	}, reviewer)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if reviewed.ReviewedBy == nil || *reviewed.ReviewedBy != 10 || reviewed.ReviewedAt == nil || reviewed.ReviewComment != "Looks consistent." {
		t.Fatalf("review fields not persisted: %+v", reviewed)
	}
	// Returning for investigation without a reason is rejected.
	_, err = svc.Transition(ctx, analysisID, dto.DeviationAnalysisTransitionRequest{
		ToState: "investigating", Comment: "   ",
	}, reviewer)
	if !errors.As(err, &appErr) || appErr.Code != util.CodeReturnReason {
		t.Fatalf("reasonless return error=%v, want return reason required", err)
	}
	returned, err := svc.Transition(ctx, analysisID, dto.DeviationAnalysisTransitionRequest{
		ToState: "investigating", Comment: "DO slope evidence needs checking",
	}, reviewer)
	if err != nil {
		t.Fatalf("return: %v", err)
	}
	if returned.ReviewedBy != nil || returned.ReviewedByName != "" || returned.ReviewedAt != nil ||
		returned.ReviewComment != "" || returned.ReturnReason != "DO slope evidence needs checking" {
		t.Fatalf("return did not clear review conclusion: %+v", returned)
	}
	// While investigating there is no review to confirm against.
	if _, err := svc.Transition(ctx, analysisID, dto.DeviationAnalysisTransitionRequest{
		ToState: "confirmed",
	}, confirmer); err == nil {
		t.Fatal("confirm from investigating unexpectedly succeeded")
	}
	// A different reviewer re-reviews; return reason is cleared and confirmation opens.
	secondReviewer := util.Actor{UserID: 12, Username: "scientist", Role: "process_scientist", RequestID: "req-rereview"}
	rereviewed, err := svc.Transition(ctx, analysisID, dto.DeviationAnalysisTransitionRequest{
		ToState: "reviewed", Comment: "Re-checked after investigation.",
	}, secondReviewer)
	if err != nil {
		t.Fatalf("re-review: %v", err)
	}
	if rereviewed.ReturnReason != "" || rereviewed.ReviewedBy == nil || *rereviewed.ReviewedBy != 12 {
		t.Fatalf("re-review did not restore eligibility: %+v", rereviewed)
	}
	// The new reviewer still cannot self-confirm; only the third person can.
	if _, err := svc.Transition(ctx, analysisID, dto.DeviationAnalysisTransitionRequest{
		ToState: "confirmed",
	}, secondReviewer); !errors.As(err, &appErr) || appErr.Code != util.CodeDutySeparation {
		t.Fatalf("self confirm after re-review error=%v", err)
	}
	confirmed, err := svc.Transition(ctx, analysisID, dto.DeviationAnalysisTransitionRequest{
		ToState: "confirmed",
	}, confirmer)
	if err != nil {
		t.Fatalf("final confirm: %v", err)
	}
	if confirmed.ConfirmedByName != "qa-lead" || confirmed.ConfirmedAt == nil {
		t.Fatalf("confirmation fields missing: %+v", confirmed)
	}
}

func TestConcurrentConfirmSucceedsOnce(t *testing.T) {
	svc, analysisID := setupWorkflowAnalysis(t)
	ctx := context.Background()
	reviewer := util.Actor{UserID: 10, Username: "reviewer", Role: "reviewer", RequestID: "req-review"}
	if _, err := svc.Transition(ctx, analysisID, dto.DeviationAnalysisTransitionRequest{
		ToState: "reviewed", Comment: "ok",
	}, reviewer); err != nil {
		t.Fatal(err)
	}
	const contenders = 8
	type outcome struct{ ok bool }
	start := make(chan struct{})
	results := make(chan outcome, contenders)
	for i := 0; i < contenders; i++ {
		go func(n int) {
			actor := util.Actor{UserID: uint(20 + n), Username: "confirmer-" + string(rune('a'+n)),
				Role: "admin", RequestID: "req-confirm-" + string(rune('a'+n))}
			<-start
			_, err := svc.Transition(ctx, analysisID, dto.DeviationAnalysisTransitionRequest{
				ToState: "confirmed",
			}, actor)
			results <- outcome{ok: err == nil}
		}(i)
	}
	close(start)
	successes := 0
	for i := 0; i < contenders; i++ {
		if (<-results).ok {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent confirm successes=%d, want exactly 1", successes)
	}
	final, err := svc.Get(ctx, analysisID)
	if err != nil {
		t.Fatal(err)
	}
	if final.AnalysisState != "confirmed" || final.ConfirmedBy == nil {
		t.Fatalf("unexpected final state: %+v", final)
	}
}

func TestConfirmRollsBackWhenAuditFails(t *testing.T) {
	db := newTestDB(t)
	base := repository.NewDeviationAnalysisRepository(db)
	failing := failingAnalysisRepository{DeviationAnalysisRepository: base, failWorkflowAudit: true}
	svc := NewDeviationAnalysisService(
		failing, repository.NewCultureRecipeRepository(db),
		repository.NewSensorSeriesRepository(db), repository.NewAuditRepository(db), algorithm.NewEvaluator(),
	)
	_, analysisID := seedCompletedAnalysis(t, db)
	ctx := context.Background()
	reviewer := util.Actor{UserID: 10, Username: "reviewer", Role: "reviewer", RequestID: "req-review"}
	if _, err := svc.Transition(ctx, analysisID, dto.DeviationAnalysisTransitionRequest{
		ToState: "reviewed", Comment: "ok",
	}, reviewer); err != nil {
		t.Fatal(err)
	}
	confirmer := util.Actor{UserID: 11, Username: "admin", Role: "admin", RequestID: "req-confirm"}
	if _, err := svc.Transition(ctx, analysisID, dto.DeviationAnalysisTransitionRequest{
		ToState: "confirmed",
	}, confirmer); err == nil {
		t.Fatal("confirm with failing audit unexpectedly succeeded")
	}
	// State, review conclusion and confirmation info must remain untouched.
	reloaded, err := base.GetByID(ctx, analysisID, false)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.AnalysisState != "reviewed" || reloaded.ConfirmedBy != nil || reloaded.ConfirmedAt != nil {
		t.Fatalf("rollback incomplete: state=%s confirmed_by=%v", reloaded.AnalysisState, reloaded.ConfirmedBy)
	}
	var auditCount int64
	if err := db.Model(&model.AuditLog{}).Where("entity_id = ? AND action = ?", analysisID, "transition").
		Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 { // only the successful review transition
		t.Fatalf("transition audit count=%d, want 1 (review only)", auditCount)
	}
}

// failingAnalysisRepository forces the audit insert inside WorkflowTransition to
// fail, exercising the all-or-nothing confirmation transaction.
type failingAnalysisRepository struct {
	repository.DeviationAnalysisRepository
	failWorkflowAudit bool
}

func (f failingAnalysisRepository) WorkflowTransition(
	ctx context.Context, id uint, from, to string, updates map[string]any, audit model.AuditLog,
) (bool, error) {
	if f.failWorkflowAudit && to == string(constants.AnalysisConfirmed) {
		return false, fmt.Errorf("forced audit failure")
	}
	return f.DeviationAnalysisRepository.WorkflowTransition(ctx, id, from, to, updates, audit)
}

func setupWorkflowAnalysis(t *testing.T) (*DeviationAnalysisService, uint) {
	t.Helper()
	db := newTestDB(t)
	svc := NewDeviationAnalysisService(
		repository.NewDeviationAnalysisRepository(db), repository.NewCultureRecipeRepository(db),
		repository.NewSensorSeriesRepository(db), repository.NewAuditRepository(db), algorithm.NewEvaluator(),
	)
	initiator, id := seedCompletedAnalysis(t, db)
	_ = initiator
	return svc, id
}

func seedCompletedAnalysis(t *testing.T, db *gorm.DB) (util.Actor, uint) {
	t.Helper()
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	vessel := model.FermentationVessel{
		VesselCode: "FV-A1", Name: "Analysis vessel", WorkingVolumeL: 100,
		SensorChannels: `["ph"]`, Location: "Lab", OwnerTeam: "Process",
		VesselState: "active", CommissionedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := repository.NewFermentationVesselRepository(db).Create(context.Background(), &vessel); err != nil {
		t.Fatal(err)
	}
	boundaries, references, tolerances := testRecipeConfig(t)
	recipe := model.CultureRecipe{
		VesselID: vessel.ID, RecipeCode: "ANALYSIS-A", Version: 1, Organism: "Test organism",
		TargetDurationH: 8, PhaseBoundariesJSON: string(boundaries), ReferenceCurvesJSON: string(references),
		ToleranceProfileJSON: string(tolerances), RecipeState: "published",
		CreatedBy: 8, CreatedByName: "scientist", CreatedAt: now, UpdatedAt: now,
	}
	if err := repository.NewCultureRecipeRepository(db).Create(context.Background(), &recipe); err != nil {
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
	if err := repository.NewSensorSeriesRepository(db).Create(context.Background(), &series); err != nil {
		t.Fatal(err)
	}
	svc := NewDeviationAnalysisService(
		repository.NewDeviationAnalysisRepository(db), repository.NewCultureRecipeRepository(db),
		repository.NewSensorSeriesRepository(db), repository.NewAuditRepository(db), algorithm.NewEvaluator(),
	)
	initiator := util.Actor{UserID: 9, Username: "analyst", Role: "data_analyst", RequestID: "req-run"}
	result, _, err := svc.Run(context.Background(), dto.RunDeviationAnalysisRequest{SensorSeriesID: series.ID}, "idem-workflow", initiator)
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return initiator, result.ID
}

func TestRunRequiresReadySeriesAndIdempotencyKey(t *testing.T) {
	db := newTestDB(t)
	svc := NewDeviationAnalysisService(
		repository.NewDeviationAnalysisRepository(db), repository.NewCultureRecipeRepository(db),
		repository.NewSensorSeriesRepository(db), repository.NewAuditRepository(db), algorithm.NewEvaluator(),
	)
	_, _, err := svc.Run(context.Background(), dto.RunDeviationAnalysisRequest{SensorSeriesID: 99}, "", util.Actor{})
	var appErr *util.AppError
	if !errors.As(err, &appErr) || appErr.Code != util.CodeIdempotency {
		t.Fatalf("missing key error=%v", err)
	}
}

func TestAnalysisRoleContract(t *testing.T) {
	if constants.HasPermission(constants.RoleDataAnalyst, constants.PermissionAnalysisConfirm) {
		t.Fatal("data analyst should not receive confirm permission")
	}
}
