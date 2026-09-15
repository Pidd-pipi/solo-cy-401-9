<template>
  <el-card class="change-card">
    <template #header>
      <div class="ch-header">
        <b>合同变更</b>
        <el-tag v-if="active" type="warning" size="small">存在待处理变更，完成确认已暂停</el-tag>
      </div>
    </template>

    <!-- Pending change -->
    <div v-if="active" class="active-change">
      <div class="ch-row">
        <span>变更单号 #{{ active.id }}</span>
        <StatusBadge :status="active.status" kind="change" />
      </div>
      <p class="muted">
        发起方：{{ active.proposerName }}（{{ partyLabel(active.proposerParty) }}） ·
        {{ active.respondedAt || active.createdAt }}
      </p>
      <p><b>变更原因：</b>{{ active.reason }}</p>
      <p><b>范围说明：</b>{{ active.scope }}</p>
      <p>
        <b>金额调整：</b>
        <span :class="active.amountDelta >= 0 ? 'delta-up' : 'delta-down'">
          {{ active.amountDelta >= 0 ? '+' : '' }}{{ formatCurrency(active.amountDelta) }}
        </span>
        <span class="muted">
          （{{ formatCurrency(active.originalAmount) }} →
          <b>{{ formatCurrency(active.newAmount) }}</b>）
        </span>
      </p>
      <el-table :data="active.proposedStages" size="small" border>
        <el-table-column prop="name" label="阶段" min-width="120" />
        <el-table-column label="调整后金额" min-width="120">
          <template #default="{ row }">{{ formatCurrency(row.amount) }}</template>
        </el-table-column>
        <el-table-column prop="status" label="状态" width="100" />
        <el-table-column prop="dueAt" label="时间节点" min-width="120" />
      </el-table>
      <div class="ch-actions">
        <template v-if="isProposer">
          <el-button type="info" :loading="acting" @click="withdraw">撤回变更</el-button>
          <span class="muted">等待合同另一方处理</span>
        </template>
        <template v-else>
          <el-button type="success" :loading="acting" @click="approve">同意并应用变更</el-button>
          <el-button type="danger" plain :loading="acting" @click="reject">拒绝</el-button>
          <span class="muted">仅合同另一方可处理</span>
        </template>
      </div>
    </div>

    <!-- No pending change: entry to raise one -->
    <div v-else class="no-change">
      <el-button
        v-if="canRaise"
        type="primary"
        plain
        :disabled="!canRaise"
        @click="openCreate"
      >
        发起合同变更
      </el-button>
      <span v-else class="muted">仅进行中或待确认完成的合同可发起变更</span>
    </div>

    <!-- History -->
    <el-divider content-position="left">变更历史</el-divider>
    <el-timeline v-if="history.length">
      <el-timeline-item
        v-for="item in history"
        :key="item.id"
        :timestamp="item.respondedAt || item.createdAt"
        placement="top"
      >
        <div class="hist-row">
          <b>#{{ item.id }} {{ item.reason }}</b>
          <StatusBadge :status="item.status" kind="change" />
        </div>
        <p class="muted">
          {{ item.proposerName }}（{{ partyLabel(item.proposerParty) }}）发起 ·
          金额 {{ formatCurrency(item.originalAmount) }} → {{ formatCurrency(item.newAmount) }}
          <template v-if="item.responderName"> · {{ item.responderName }} 处理</template>
        </p>
      </el-timeline-item>
    </el-timeline>
    <el-empty v-else description="暂无变更记录" :image-size="60" />

    <!-- Create dialog -->
    <el-dialog v-model="dialogVisible" title="发起合同变更" width="640px">
      <el-form label-position="top">
        <el-form-item label="变更原因" required>
          <el-input v-model="form.reason" type="textarea" :rows="2" maxlength="500" show-word-limit />
        </el-form-item>
        <el-form-item label="范围说明" required>
          <el-input v-model="form.scope" type="textarea" :rows="2" maxlength="1000" show-word-limit />
        </el-form-item>
        <el-form-item label="金额增减（元，负数为减）">
          <el-input-number v-model="form.amountDelta" :step="1000" :precision="2" style="width: 220px" />
          <span class="muted form-hint">
            原总额 {{ formatCurrency(contract.totalAmount) }}，新总额
            <b>{{ formatCurrency(newTotal) }}</b>
          </span>
        </el-form-item>
        <el-form-item label="阶段金额调整（已完成阶段冻结，仅可调整未完成阶段）" required>
          <el-table :data="form.stages" size="small" border style="width: 100%">
            <el-table-column label="阶段" min-width="120">
              <template #default="{ row, $index }">
                <el-input v-model="row.name" size="small" :disabled="isFrozen($index)" />
              </template>
            </el-table-column>
            <el-table-column label="金额（元）" min-width="140">
              <template #default="{ row, $index }">
                <el-input-number
                  v-model="row.amount"
                  :min="0"
                  :precision="2"
                  :controls="false"
                  size="small"
                  :disabled="isFrozen($index)"
                  style="width: 120px"
                />
              </template>
            </el-table-column>
            <el-table-column label="状态" width="120">
              <template #default="{ row, $index }">
                <el-select v-model="row.status" size="small" :disabled="isFrozen($index)">
                  <el-option v-if="isFrozen($index)" label="已完成" value="done" />
                  <template v-else>
                    <el-option label="待开始" value="pending" />
                    <el-option label="进行中" value="in_progress" />
                  </template>
                </el-select>
              </template>
            </el-table-column>
            <el-table-column label="时间节点" min-width="130">
              <template #default="{ row, $index }"><el-input v-model="row.dueAt" size="small" :disabled="isFrozen($index)" /></template>
            </el-table-column>
          </el-table>
        </el-form-item>
        <div class="conservation">
          <span>已完成金额合计：<b>{{ formatCurrency(doneSum) }}</b></span>
          <span>未完成阶段合计：<b :class="conservationOk ? 'ok' : 'bad'">{{ formatCurrency(unfinishedSum) }}</b></span>
          <span>新总额：<b :class="conservationOk ? 'ok' : 'bad'">{{ formatCurrency(newTotal) }}</b></span>
          <el-tag v-if="conservationOk" type="success" size="small">金额守恒</el-tag>
          <el-tag v-else type="danger" size="small">已完成 + 未完成 ≠ 新总额</el-tag>
        </div>
      </el-form>
      <template #footer>
        <el-button @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" :disabled="!canSubmit" :loading="submitting" @click="submit">
          提交变更
        </el-button>
      </template>
    </el-dialog>
  </el-card>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue';
import { ElMessage, ElMessageBox } from 'element-plus';
import type { Contract, ContractChange, ContractStage } from '../../types';
import { contractApi } from '../../api/contract';
import { ContractChangePartyLabel } from '../../types/enums';
import StatusBadge from './StatusBadge.vue';
import { formatCurrency } from '../../utils/formatCurrency';

const props = defineProps<{ contract: Contract; currentUserId: number }>();
const emit = defineEmits<{ changed: [] }>();

const history = ref<ContractChange[]>([]);
const acting = ref(false);
const dialogVisible = ref(false);
const submitting = ref(false);

const active = computed(() => props.contract.activeChange ?? null);
const isProposer = computed(() => active.value?.proposerId === props.currentUserId);
const canRaise = computed(
  () =>
    !active.value &&
    (props.contract.status === 'in_progress' || props.contract.status === 'pending_review')
);

const form = ref<{ reason: string; scope: string; amountDelta: number; stages: ContractStage[] }>({
  reason: '',
  scope: '',
  amountDelta: 0,
  stages: []
});

const newTotal = computed(() => Math.round((props.contract.totalAmount + form.value.amountDelta) * 100) / 100);
const doneSum = computed(() =>
  Math.round(
    form.value.stages
      .filter((s) => s.status === 'done')
      .reduce((sum, s) => sum + (Number(s.amount) || 0), 0) * 100
  ) / 100
);
const unfinishedSum = computed(() =>
  Math.round(
    form.value.stages
      .filter((s) => s.status !== 'done')
      .reduce((sum, s) => sum + (Number(s.amount) || 0), 0) * 100
  ) / 100
);
// Freezing is anchored to the ORIGINAL contract snapshot: a stage settled
// before the change was raised can never be edited, regardless of what the form
// row status currently shows.
function isFrozen(index: number): boolean {
  return props.contract.stages[index]?.status === 'done';
}
const conservationOk = computed(
  () =>
    Math.abs(doneSum.value + unfinishedSum.value - newTotal.value) < 0.01 &&
    newTotal.value >= 0 &&
    doneSumOk.value
);
// Done rows must retain their original name/amount/status.
const doneSumOk = computed(() =>
  form.value.stages.every((s, i) => {
    if (!isFrozen(i)) return true;
    const orig = props.contract.stages[i];
    return (
      s.status === 'done' &&
      s.name === orig.name &&
      Math.abs((Number(s.amount) || 0) - orig.amount) < 0.01
    );
  })
);
const canSubmit = computed(
  () =>
    form.value.reason.trim().length >= 2 &&
    form.value.scope.trim().length >= 2 &&
    form.value.stages.length > 0 &&
    form.value.stages.every((s, i) => isFrozen(i) || s.status !== 'done') &&
    conservationOk.value
);

function partyLabel(party: string): string {
  return ContractChangePartyLabel[party] || party;
}

async function loadHistory() {
  history.value = await contractApi.listChanges(props.contract.id);
}

function openCreate() {
  form.value = {
    reason: '',
    scope: '',
    amountDelta: 0,
    stages: props.contract.stages.map((s) => ({ ...s }))
  };
  dialogVisible.value = true;
}

async function submit() {
  submitting.value = true;
  try {
    await contractApi.createChange(props.contract.id, {
      reason: form.value.reason.trim(),
      scope: form.value.scope.trim(),
      amountDelta: form.value.amountDelta,
      stages: form.value.stages
    });
    ElMessage.success('变更单已提交，等待另一方处理');
    dialogVisible.value = false;
    emit('changed');
  } finally {
    submitting.value = false;
  }
}

async function approve() {
  if (!active.value) return;
  await ElMessageBox.confirm('同意后将同步更新合同总额与阶段金额，确认继续？', '同意合同变更', {
    type: 'warning'
  });
  acting.value = true;
  try {
    await contractApi.approveChange(props.contract.id, active.value.id);
    ElMessage.success('已同意并应用变更');
    emit('changed');
  } finally {
    acting.value = false;
  }
}

async function reject() {
  if (!active.value) return;
  await ElMessageBox.confirm('拒绝后合同保持原样，确认拒绝该变更？', '拒绝合同变更', {
    type: 'warning'
  });
  acting.value = true;
  try {
    await contractApi.rejectChange(props.contract.id, active.value.id);
    ElMessage.success('已拒绝变更');
    emit('changed');
  } finally {
    acting.value = false;
  }
}

async function withdraw() {
  if (!active.value) return;
  await ElMessageBox.confirm('撤回后合同保持原样，确认撤回该变更？', '撤回合同变更', {
    type: 'warning'
  });
  acting.value = true;
  try {
    await contractApi.withdrawChange(props.contract.id, active.value.id);
    ElMessage.success('已撤回变更');
    emit('changed');
  } finally {
    acting.value = false;
  }
}

onMounted(() => void loadHistory());
defineExpose({ reload: loadHistory });
</script>

<style scoped>
.change-card { margin-bottom: 24px; }
.ch-header { display: flex; justify-content: space-between; align-items: center; }
.ch-row { display: flex; justify-content: space-between; align-items: center; margin-bottom: 6px; }
.ch-actions { margin-top: 14px; display: flex; gap: 10px; align-items: center; }
.delta-up { color: #c45656; font-weight: 600; }
.delta-down { color: #2ba471; font-weight: 600; }
.hist-row { display: flex; justify-content: space-between; align-items: center; }
.form-hint { margin-left: 12px; }
.conservation { display: flex; gap: 20px; align-items: center; margin-top: 8px; }
.ok { color: #2ba471; }
.bad { color: #c45656; }
</style>
