// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { IERC165 } from "@openzeppelin/contracts/utils/introspection/IERC165.sol";
import { AccessControlEnumerableUpgradeable } from "@openzeppelin/contracts-upgradeable/access/extensions/AccessControlEnumerableUpgradeable.sol";
import { OwnableUpgradeable } from "@openzeppelin/contracts-upgradeable/access/OwnableUpgradeable.sol";
import { PausableUpgradeable } from "@openzeppelin/contracts-upgradeable/utils/PausableUpgradeable.sol";
import { ReentrancyGuardUpgradeable } from "@openzeppelin/contracts-upgradeable/utils/ReentrancyGuardUpgradeable.sol";
import { NilConstants } from "../../common/libraries/NilConstants.sol";
import { StorageUtils } from "../../common/libraries/StorageUtils.sol";
import { INilGasPriceOracle } from "./interfaces/INilGasPriceOracle.sol";
import { NilAccessControlUpgradeable } from "../../NilAccessControlUpgradeable.sol";
import { IRelayMessage } from "./interfaces/IRelayMessage.sol";
import { IFeeStorage } from "./interfaces/IFeeStorage.sol";

// solhint-disable reason-string
contract NilGasPriceOracle is OwnableUpgradeable, PausableUpgradeable, NilAccessControlUpgradeable, INilGasPriceOracle {
  using StorageUtils for bytes32;

  /*//////////////////////////////////////////////////////////////////////////
                             STATE VARIABLES   
    //////////////////////////////////////////////////////////////////////////*/

  /// @notice The latest known maxFeePerGas.
  uint256 public override maxFeePerGas;

  /// @notice The latest known maxPriorityFeePerGas.
  uint256 public override maxPriorityFeePerGas;

  /*//////////////////////////////////////////////////////////////////////////
                             CONSTRUCTOR   
    //////////////////////////////////////////////////////////////////////////*/

  /// @custom:oz-upgrades-unsafe-allow constructor
  constructor() {
    _disableInitializers();
  }

  function initialize(
    address _owner,
    address _defaultAdmin,
    address _proposer,
    uint64 _maxFeePerGas,
    uint64 _maxPriorityFeePerGas
  ) public initializer {
    // Validate input parameters
    if (_owner == address(0)) {
      revert ErrorInvalidOwner();
    }

    if (_defaultAdmin == address(0)) {
      revert ErrorInvalidDefaultAdmin();
    }

    // Initialize the Ownable contract with the owner address
    OwnableUpgradeable.__Ownable_init(_owner);

    // Initialize the Pausable contract
    PausableUpgradeable.__Pausable_init();

    // Initialize the AccessControlEnumerable contract
    __AccessControlEnumerable_init();

    // Set role admins
    // The OWNER_ROLE is set as its own admin to ensure that only the current owner can manage this role.
    _setRoleAdmin(NilConstants.OWNER_ROLE, NilConstants.OWNER_ROLE);

    // The DEFAULT_ADMIN_ROLE is set as its own admin to ensure that only the current default admin can manage this
    // role.
    _setRoleAdmin(DEFAULT_ADMIN_ROLE, NilConstants.OWNER_ROLE);

    // Grant roles to defaultAdmin and owner
    // The DEFAULT_ADMIN_ROLE is granted to both the default admin and the owner to ensure that both have the
    // highest level of control.
    _grantRole(NilConstants.OWNER_ROLE, _owner);
    _grantRole(DEFAULT_ADMIN_ROLE, _defaultAdmin);

    _grantRole(NilConstants.PROPOSER_ROLE_ADMIN, _defaultAdmin);
    _grantRole(NilConstants.PROPOSER_ROLE_ADMIN, _owner);

    // Grant proposer to defaultAdmin and owner
    // The PROPOSER_ROLE is granted to the default admin and the owner.
    // This ensures that both the default admin and the owner have the necessary permissions to perform
    // set GasPrice parameters if needed. This redundancy provides a fallback mechanism
    _grantRole(NilConstants.PROPOSER_ROLE, _owner);
    _grantRole(NilConstants.PROPOSER_ROLE, _defaultAdmin);

    // grant GasPriceSetter role to gasPriceSetter address
    _grantRole(NilConstants.PROPOSER_ROLE, _proposer);

    _setOracleFee(_maxFeePerGas, _maxPriorityFeePerGas);
  }

  /*//////////////////////////////////////////////////////////////////////////
                             PUBLIC RESTRICTED MUTATION FUNCTIONS   
    //////////////////////////////////////////////////////////////////////////*/

  /// @inheritdoc IFeeStorage
  function setOracleFee(uint256 newMaxFeePerGas, uint256 newMaxPriorityFeePerGas) external onlyProposer {
    _setOracleFee(newMaxFeePerGas, newMaxPriorityFeePerGas);
  }

  function _setOracleFee(uint256 _newMaxFeePerGas, uint256 _newMaxPriorityFeePerGas) internal {
    maxFeePerGas = _newMaxFeePerGas;
    maxPriorityFeePerGas = _newMaxPriorityFeePerGas;
    emit OracleFeeUpdated(_newMaxFeePerGas, _newMaxPriorityFeePerGas);
  }

  /*//////////////////////////////////////////////////////////////////////////
                             PUBLIC CONSTANT FUNCTIONS   
    //////////////////////////////////////////////////////////////////////////*/

  /// @inheritdoc IFeeStorage
  function getOracleFee() public view returns (uint256, uint256) {
    return (maxFeePerGas, maxPriorityFeePerGas);
  }

  /// @inheritdoc INilGasPriceOracle
  function computeFeeCredit(
    uint256 nilGasLimit,
    uint256 userMaxFeePerGas,
    uint256 userMaxPriorityFeePerGas
  ) public view returns (IRelayMessage.FeeCreditData memory) {
    if (nilGasLimit == 0) {
      revert ErrorInvalidGasLimitForFeeCredit();
    }

    uint256 _maxFeePerGas = userMaxFeePerGas > 0 ? userMaxFeePerGas : maxFeePerGas;

    if (_maxFeePerGas == 0) {
      revert ErrorInvalidMaxFeePerGas();
    }

    uint256 _maxPriorityFeePerGas = userMaxPriorityFeePerGas > 0 ? userMaxPriorityFeePerGas : maxPriorityFeePerGas;

    if (_maxPriorityFeePerGas == 0) {
      revert ErrorInvalidMaxPriorityFeePerGas();
    }

    return
      IRelayMessage.FeeCreditData({
        nilGasLimit: nilGasLimit,
        maxFeePerGas: _maxFeePerGas,
        maxPriorityFeePerGas: _maxPriorityFeePerGas,
        feeCredit: nilGasLimit * _maxFeePerGas
      });
  }

  /// @inheritdoc IERC165
  function supportsInterface(
    bytes4 interfaceId
  ) public view override(AccessControlEnumerableUpgradeable, IERC165) returns (bool) {
    return interfaceId == type(INilGasPriceOracle).interfaceId || super.supportsInterface(interfaceId);
  }

  /**
   * @dev Returns the current implementation address.
   */
  function getImplementation() public view override returns (address) {
    return StorageUtils.getImplementationAddress(NilConstants.IMPLEMENTATION_SLOT);
  }
}
