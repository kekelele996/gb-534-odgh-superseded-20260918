<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { CheckCircle2, FileSearch, Play, RefreshCw, RotateCcw, SearchCheck, ShieldCheck, Undo2 } from 'lucide-vue-next'
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
import type { AnalysisState } from '../types/deviation-analysis'

const analyses = useAnalysisStore()
const series = useSeriesStore()
const { auth, canRunAnalysis, canReview, canConfirm } = useAuth()
const runner = useAnalysisRun()
const drawer = ref(false)
const reviewComment = ref('')

const selected = computed(() => analyses.selected)
const isInitiator = computed(() => selected.value?.initiated_by === auth.user?.id)
const isReviewer = computed(() => selected.value?.reviewed_by === auth.user?.id)
const isConfirmer = computed(() => selected.value?.confirmed_by === auth.user?.id)
const state = computed(() => selected.value?.analysis_state)

const canMarkReviewed = computed(() =>
  Boolean(canReview.value && (state.value === 'completed' || state.value === 'investigating') && !isInitiator.value))
const canReturnForInvestigation = computed(() =>
  Boolean(canReview.value && state.value === 'reviewed'))
const canConfirmNow = computed(() =>
  Boolean(canConfirm.value && state.value === 'reviewed' && !isInitiator.value && !isReviewer.value))
const canVoidNow = computed(() =>
  Boolean(canReview.value && ['completed', 'reviewed', 'investigating', 'confirmed'].includes(state.value ?? '')))

const confirmBlockReason = computed(() => {
  if (state.value !== 'reviewed' || !canConfirm.value) return ''
  if (isInitiator.value) return '您是本分析的发起人，三人分离规则要求确认人必须独立于发起人与复核人。'
  if (isReviewer.value) return '您已标记复核，不能再确认同一结果；请由第三位独立人员确认。'
  return ''
})

function formatTime(value?: string) {
  return value ? new Date(value).toLocaleString() : ''
}

async function run() {
  try { await runner.run(); ElMessage.success('分析已完成或返回现有幂等结果') }
  catch (error) { ElMessage.error(error instanceof Error ? error.message : '分析运行失败') }
}
async function submitReview() {
  if (!reviewComment.value.trim()) {
    ElMessage.warning('请填写复核结论后再标记已复核')
    return
  }
  try { await analyses.transition('reviewed', reviewComment.value); reviewComment.value = ''; ElMessage.success('复核结论已记录') }
  catch (error) { ElMessage.error(error instanceof Error ? error.message : '状态更新失败') }
}
async function returnForInvestigation() {
  try {
    const { value } = await ElMessageBox.prompt('请填写退回调查原因，原复核结论将被清空，重新复核后才可确认。', '退回调查', {
      confirmButtonText: '确认退回', cancelButtonText: '取消', inputType: 'textarea',
      inputValidator: (input) => Boolean(input && input.trim()) || '退回原因不能为空',
    })
    await analyses.transition('investigating', value)
    ElMessage.success('已退回调查，等待重新复核')
  } catch (error) {
    if (error !== 'cancel' && error instanceof Error) ElMessage.error(error.message)
  }
}
async function confirmResult() {
  try { await analyses.transition('confirmed', ''); ElMessage.success('确认信息与审计记录已一次性写入') }
  catch (error) { ElMessage.error(error instanceof Error ? error.message : '确认失败') }
}
async function voidAnalysis() {
  try {
    const { value } = await ElMessageBox.prompt('请填写作废说明', '作废分析', {
      confirmButtonText: '确认作废', cancelButtonText: '取消', inputType: 'textarea',
      inputValidator: (input) => Boolean(input && input.trim()) || '作废说明不能为空',
    })
    await analyses.transition('voided', value)
    ElMessage.success('分析已作废')
  } catch (error) {
    if (error !== 'cancel' && error instanceof Error) ElMessage.error(error.message)
  }
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
        <el-button v-if="analyses.selected" @click="drawer = true"><FileSearch :size="16" />查看解释</el-button>
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
          <button v-for="item in analyses.items" v-else :key="item.id" class="analysis-row" :class="{ selected: analyses.selected?.id === item.id }" @click="analyses.selected = item">
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

            <section class="duty-band">
              <div class="duty-title"><ShieldCheck :size="18" /><strong>三人分离职责链</strong><small>发起、复核、确认必须由不同人员完成</small></div>
              <ol class="duty-roles">
                <li :class="{ self: isInitiator, done: true }">
                  <span class="duty-step">发起人</span>
                  <strong>{{ selected.initiated_by_name }}</strong>
                  <small v-if="isInitiator">当前操作者</small>
                </li>
                <li :class="{ self: isReviewer, done: Boolean(selected.reviewed_by_name), returned: state === 'investigating' }">
                  <span class="duty-step">复核人</span>
                  <strong v-if="selected.reviewed_by_name">{{ selected.reviewed_by_name }}<small v-if="selected.reviewed_at"> · {{ formatTime(selected.reviewed_at) }}</small></strong>
                  <strong v-else-if="state === 'investigating'">已退回 · 待重新复核</strong>
                  <strong v-else>待复核</strong>
                  <small v-if="isReviewer">当前操作者</small>
                </li>
                <li :class="{ self: isConfirmer, done: Boolean(selected.confirmed_by_name) }">
                  <span class="duty-step">确认人</span>
                  <strong v-if="selected.confirmed_by_name">{{ selected.confirmed_by_name }}<small v-if="selected.confirmed_at"> · {{ formatTime(selected.confirmed_at) }}</small></strong>
                  <strong v-else>待确认</strong>
                  <small v-if="isConfirmer">当前操作者</small>
                </li>
              </ol>
              <el-alert
                v-if="state === 'investigating' && selected.return_reason"
                type="warning" :closable="false" show-icon
                title="退回调查原因" :description="selected.return_reason"
              />
              <el-alert
                v-if="state === 'reviewed' && selected.review_comment"
                type="info" :closable="false" show-icon
                title="复核结论" :description="selected.review_comment"
              />
            </section>

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
            <section v-if="canReview || canConfirm" class="review-band">
              <div><SearchCheck :size="19" /><span><strong>人工审阅</strong><small>操作按当前登录角色与职责分离规则开放</small></span></div>
              <el-input
                v-if="canMarkReviewed"
                v-model="reviewComment"
                type="textarea" :rows="2"
                :placeholder="state === 'investigating' ? '重新复核结论（必填），提交后恢复确认资格' : '复核结论（必填）'"
              />
              <el-alert v-else-if="confirmBlockReason" :title="confirmBlockReason" type="warning" :closable="false" show-icon />
              <div class="review-actions">
                <el-button v-if="canMarkReviewed" type="primary" plain @click="submitReview">
                  <SearchCheck :size="16" />{{ state === 'investigating' ? '重新复核' : '标记已复核' }}
                </el-button>
                <el-button v-if="canReturnForInvestigation" plain @click="returnForInvestigation">
                  <Undo2 :size="16" />退回调查
                </el-button>
                <el-button v-if="canConfirmNow" type="primary" @click="confirmResult"><CheckCircle2 :size="16" />确认结果</el-button>
                <el-button v-if="canVoidNow" text @click="voidAnalysis">作废</el-button>
              </div>
              <p v-if="!canMarkReviewed && !canReturnForInvestigation && !canConfirmNow && !canVoidNow && !confirmBlockReason" class="muted">
                当前状态下没有可由您执行的操作。
              </p>
            </section>
          </template>
          <div v-else class="empty-state"><h2>选择历史结果</h2><p>对齐曲线和阶段证据将在此显示。</p></div>
        </section>
      </div>
    </div>
    <AnalysisExplanationDrawer v-model="drawer" :analysis="analyses.selected" />
  </AppShell>
</template>
