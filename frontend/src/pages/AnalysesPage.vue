<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { CheckCircle2, FileSearch, Play, RefreshCw, RotateCcw, SearchCheck, Undo2, Ban } from 'lucide-vue-next'
import AnalysisExplanationDrawer from '../components/common/AnalysisExplanationDrawer.vue'
import AppShell from '../components/common/AppShell.vue'
import DeviationBadge from '../components/common/DeviationBadge.vue'
import KineticsChart from '../components/common/KineticsChart.vue'
import PageHeader from '../components/common/PageHeader.vue'
import PhaseBadge from '../components/common/PhaseBadge.vue'
import StateBadge from '../components/common/StateBadge.vue'
import { useAnalysisRun } from '../hooks/useAnalysisRun'
import { useAuth } from '../hooks/useAuth'
import { useAnalysisStore } from '../stores/deviation-analysis'
import { useSeriesStore } from '../stores/sensor-series'
import type { AnalysisAction, AnalysisState } from '../types/deviation-analysis'

const analyses = useAnalysisStore()
const series = useSeriesStore()
const { canRunAnalysis, canReview, canConfirm } = useAuth()
const runner = useAnalysisRun()
const drawer = ref(false)
const reviewComment = ref('')
const returnReason = ref('')
const confirmComment = ref('')

const selected = computed(() => analyses.selected)
const canAct = (action: AnalysisAction) => selected.value?.available_actions.includes(action) ?? false
const showWorkflow = computed(() => canReview.value || canConfirm.value)
const reviewReady = computed(() => reviewComment.value.trim().length > 0)
const returnReady = computed(() => returnReason.value.trim().length > 0)
const formatTime = (value?: string) => (value ? new Date(value).toLocaleString() : '—')

async function run() {
  try { await runner.run(); ElMessage.success('分析已完成或返回现有幂等结果') }
  catch (error) { ElMessage.error(error instanceof Error ? error.message : '分析运行失败') }
}
async function transition(state: AnalysisState, comment: string) {
  if (!selected.value) return
  try {
    await analyses.transition(state, comment)
    reviewComment.value = ''
    returnReason.value = ''
    confirmComment.value = ''
    ElMessage.success('分析状态已更新')
  }
  catch (error) { ElMessage.error(error instanceof Error ? error.message : '状态更新失败') }
}
async function submitReview() {
  if (!reviewReady.value) { ElMessage.warning('请填写复核结论'); return }
  await transition('reviewed', reviewComment.value)
}
async function submitReturn() {
  if (!returnReady.value) { ElMessage.warning('请填写退回调查原因'); return }
  await transition('investigating', returnReason.value)
}
async function submitConfirm() { await transition('confirmed', confirmComment.value) }
async function voidAnalysis() {
  try { await analyses.transition('voided', ''); ElMessage.success('分析已作废') }
  catch (error) { ElMessage.error(error instanceof Error ? error.message : '作废失败') }
}
async function replay() {
  try { await analyses.replay(); ElMessage.success('冻结输入重放一致') }
  catch (error) { ElMessage.error(error instanceof Error ? error.message : '重放失败') }
}
onMounted(async () => { await Promise.all([series.load(), analyses.load()]); runner.seriesId.value = runner.readySeries.value[0]?.id })
</script>

<template>
  <AppShell>
    <div class="page-wrap">
      <PageHeader eyebrow="PHASE-CONSTRAINED DTW" title="偏差分析" description="对齐实测与配方参考曲线，逐阶段呈现持续时间、斜率、峰值时间与曲线距离证据。">
        <el-tooltip content="刷新数据"><el-button circle aria-label="刷新" @click="analyses.load()"><RefreshCw :size="17" /></el-button></el-tooltip>
        <el-button v-if="selected" @click="drawer = true"><FileSearch :size="16" />查看解释</el-button>
      </PageHeader>
      <section v-if="canRunAnalysis" class="run-band">
        <div><Play :size="20" /><span><strong>运行冻结分析</strong><small>相同输入哈希与算法版本返回同一历史结果</small></span></div>
        <el-select v-model="runner.seriesId.value" placeholder="选择就绪时序">
          <el-option v-for="item in runner.readySeries.value" :key="item.id" :label="`${item.run_code} · ${item.recipe?.recipe_code ?? ''}`" :value="item.id" />
        </el-select>
        <el-button type="primary" :loading="runner.running.value" :disabled="!runner.seriesId.value" @click="run"><Play :size="16" />运行分析</el-button>
      </section>
      <el-alert v-if="analyses.error" :title="analyses.error" type="error" :closable="false" show-icon />
      <div class="analysis-workspace">
        <section class="analysis-list">
          <div class="section-heading"><div><h2>历史结果</h2><p>{{ analyses.items.length }} 条不可覆盖记录</p></div></div>
          <el-skeleton v-if="analyses.loading" :rows="6" animated />
          <button v-for="item in analyses.items" v-else :key="item.id" class="analysis-row" :class="{ selected: selected?.id === item.id }" @click="analyses.selected = item">
            <span>#{{ item.id }}</span>
            <span><strong>{{ item.sensor_series?.run_code ?? 'Series ' + item.sensor_series_id }}</strong><small>{{ new Date(item.analyzed_at).toLocaleString() }}</small></span>
            <DeviationBadge :level="item.deviation_level" />
            <StateBadge :state="item.analysis_state" />
          </button>
          <div v-if="!analyses.loading && !analyses.items.length" class="empty-inline">暂无分析结果</div>
        </section>
        <section class="analysis-detail">
          <template v-if="selected">
            <div class="analysis-summary">
              <div><p class="eyebrow">DEVIATION RESULT</p><h2>{{ selected.sensor_series?.run_code }}</h2><small>{{ selected.algorithm_version }}</small></div>
              <DeviationBadge :level="selected.deviation_level" />
              <StateBadge :state="selected.analysis_state" />
              <el-tooltip v-if="canRunAnalysis" content="重放冻结输入"><el-button circle aria-label="重放分析" @click="replay"><RotateCcw :size="17" /></el-button></el-tooltip>
            </div>

            <section class="role-trail" aria-label="三方角色">
              <article :class="{ active: true }">
                <p class="eyebrow">发起人 INITIATOR</p>
                <strong>{{ selected.initiated_by_name }}</strong>
                <small>{{ formatTime(selected.analyzed_at) }}</small>
              </article>
              <article :class="{ active: Boolean(selected.reviewed_by), pending: !selected.reviewed_by }">
                <p class="eyebrow">复核人 REVIEWER</p>
                <strong>{{ selected.reviewed_by_name || '待复核' }}</strong>
                <small>{{ formatTime(selected.reviewed_at) }}</small>
              </article>
              <article :class="{ active: Boolean(selected.confirmed_by), pending: !selected.confirmed_by }">
                <p class="eyebrow">确认人 CONFIRMER</p>
                <strong>{{ selected.confirmed_by_name || '待确认' }}</strong>
                <small>{{ formatTime(selected.confirmed_at) }}</small>
              </article>
            </section>

            <el-alert
              v-if="selected.return_reason"
              class="return-alert"
              type="warning"
              :closable="false"
              show-icon
              title="已退回调查"
              :description="`退回原因：${selected.return_reason}。原复核结论已清空，需由非发起人重新复核后才恢复确认资格。`"
            />
            <el-alert
              v-else-if="selected.analysis_state === 'reviewed' && selected.review_comment"
              class="return-alert"
              type="success"
              :closable="false"
              show-icon
              :title="`复核结论（${selected.reviewed_by_name}）`"
              :description="selected.review_comment"
            />

            <KineticsChart :aligned="selected.aligned_curve_json" :height="380" />
            <div class="phase-score-grid">
              <article v-for="score in selected.phase_scores_json" :key="score.phase">
                <PhaseBadge :phase="score.phase" />
                <strong>{{ (score.weighted_deviation * 100).toFixed(1) }}%</strong>
                <dl>
                  <div><dt>曲线</dt><dd>{{ score.curve_distance.toFixed(3) }}</dd></div>
                  <div><dt>斜率</dt><dd>{{ score.slope_deviation.toFixed(3) }}</dd></div>
                  <div><dt>峰值</dt><dd>{{ score.peak_time_deviation.toFixed(3) }}</dd></div>
                </dl>
              </article>
            </div>
            <section v-if="showWorkflow" class="review-band">
              <div><SearchCheck :size="19" /><span><strong>人工审阅与确认</strong><small>发起、复核、确认三人分离；可用操作按当前登录者显示</small></span></div>

              <div v-if="canAct('review')" class="workflow-field">
                <label>复核结论（必填，发起人不能复核本人结果）</label>
                <el-input v-model="reviewComment" type="textarea" :rows="2" placeholder="填写复核结论后标记已复核" />
                <el-button type="primary" plain :disabled="!reviewReady" @click="submitReview"><SearchCheck :size="16" />标记已复核</el-button>
              </div>

              <template v-if="selected.analysis_state === 'reviewed'">
                <div v-if="canAct('return_investigation')" class="workflow-field">
                  <label>退回调查原因（必填）</label>
                  <el-input v-model="returnReason" type="textarea" :rows="2" placeholder="退回将清空原复核结论，重新复核后方可确认" />
                  <el-button type="warning" plain :disabled="!returnReady" @click="submitReturn"><Undo2 :size="16" />退回调查</el-button>
                </div>
                <div v-if="canAct('confirm')" class="workflow-field">
                  <label>确认备注（可选）</label>
                  <el-input v-model="confirmComment" type="textarea" :rows="2" placeholder="确认人不能是发起人或本次复核人" />
                  <el-button type="primary" @click="submitConfirm"><CheckCircle2 :size="16" />确认结果</el-button>
                </div>
                <p v-if="!canAct('confirm') && !canAct('return_investigation')" class="workflow-note">
                  当前状态为已复核；你不是本结果的有效确认人（需区别于发起人与复核人）。
                </p>
              </template>

              <div class="review-actions">
                <el-button v-if="canAct('void')" text type="danger" @click="voidAnalysis"><Ban :size="15" />作废</el-button>
              </div>
            </section>
          </template>
          <div v-else class="empty-state"><h2>选择历史结果</h2><p>对齐曲线和阶段证据将在此显示。</p></div>
        </section>
      </div>
    </div>
    <AnalysisExplanationDrawer v-model="drawer" :analysis="selected" />
  </AppShell>
</template>
